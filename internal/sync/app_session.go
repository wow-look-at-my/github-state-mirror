package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// Session is authenticated refresh identity: a context carrying a GitHub token and cache partition, plus the installation account it belongs to.
// see docs/reveal-layer.md
type Session struct {
	Ctx            context.Context
	Owner          string // installation account login (Organization or User)
	AccountType    string // "Organization" or "User"
	InstallationID int64
}

// SessionFunc yields the authenticated sessions to refresh each cycle, called fresh so short-lived credentials can be re-minted.
type SessionFunc func(ctx context.Context) ([]Session, error)

type IdentityRecorder func(ctx context.Context, principal, name string)

// AppSessions returns a SessionFunc that signs in as a GitHub App, stable "app-installation:<id>" session per installation.
// see docs/reveal-layer.md
func AppSessions(app *ghclient.AppAuthenticator, record IdentityRecorder) SessionFunc {
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
			principal := AppInstallationActor(inst.ID)
			sctx := ghclient.WithToken(ctx, token)
			sctx = actor.WithActor(sctx, principal)
			if inst.Account.Login != "" {
				sctx = actor.WithName(sctx, inst.Account.Login)
				if record != nil {
					record(sctx, principal, inst.Account.Login)
				}
			}
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
