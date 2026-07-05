package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// Session is one authenticated refresh identity: a context carrying a GitHub
// token (ghclient.WithToken) and a cache partition (actor.WithActor), plus the
// installation account it belongs to -- the OWNER whose repos the periodic
// refresher fetches. Carrying the owner is what lets the refresher name the
// resource to fetch instead of only re-fetching resources that already have a
// freshness row (the old shape returned bare contexts, so a fresh installation
// with no pre-existing cache_metadata row was never synced at all).
type Session struct {
	Ctx            context.Context
	Owner          string // installation account login (Organization or User)
	AccountType    string // "Organization" or "User"
	InstallationID int64
}

// SessionFunc yields the authenticated sessions to refresh on each cycle. It
// is called fresh each cycle so short-lived credentials (e.g. GitHub App
// installation tokens) can be re-minted. A nil SessionFunc, or one returning
// no sessions, disables periodic refreshing -- per-request data still works
// via the caller's own token.
type SessionFunc func(ctx context.Context) ([]Session, error)

// AppSessions returns a SessionFunc that signs in as a GitHub App. On each
// refresh cycle it enumerates the app's installations and mints a fresh
// installation access token for each (the tokens are short-lived, so they must
// be re-minted every cycle).
//
// Each session is partitioned under a stable "app-installation:<id>" actor —
// not the token fingerprint — so the cache bucket survives hourly token
// rotation. This key can never collide with a per-user partition (those are
// 64-char hex SHA-256 token fingerprints), keeping background-refreshed data
// out of any caller's view.
func AppSessions(app *ghclient.AppAuthenticator) SessionFunc {
	return func(ctx context.Context) ([]Session, error) {
		installs, err := app.Installations(ctx)
		if err != nil {
			return nil, fmt.Errorf("list installations: %w", err)
		}
		sessions := make([]Session, 0, len(installs))
		for _, inst := range installs {
			token, err := app.InstallationToken(ctx, inst.ID)
			if err != nil {
				slog.Warn("app session: mint installation token failed",
					"installation", inst.ID, "account", inst.Account.Login, "error", err)
				continue
			}
			sctx := ghclient.WithToken(ctx, token)
			sctx = actor.WithActor(sctx, AppInstallationActor(inst.ID))
			sessions = append(sessions, Session{
				Ctx:            sctx,
				Owner:          inst.Account.Login,
				AccountType:    inst.Account.Type,
				InstallationID: inst.ID,
			})
		}
		return sessions, nil
	}
}

// appInstallationActorPrefix marks the stable cache-partition keys of GitHub
// App installation sessions. The org-repos fetcher branches on it: an
// app-installation principal's fetch must use the owner-agnostic GraphQL query
// (an installation account can be a User), while every other principal keeps
// the identity-locked organization query.
const appInstallationActorPrefix = "app-installation:"

// AppInstallationActor returns the stable cache-partition key for a GitHub App
// installation.
func AppInstallationActor(installID int64) string {
	return appInstallationActorPrefix + fmt.Sprintf("%d", installID)
}

// IsAppInstallationActor reports whether a principal key is an App
// installation session's.
func IsAppInstallationActor(principal string) bool {
	return strings.HasPrefix(principal, appInstallationActorPrefix)
}
