package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

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
	return func(ctx context.Context) ([]context.Context, error) {
		installs, err := app.Installations(ctx)
		if err != nil {
			return nil, fmt.Errorf("list installations: %w", err)
		}
		sessions := make([]context.Context, 0, len(installs))
		for _, inst := range installs {
			token, err := app.InstallationToken(ctx, inst.ID)
			if err != nil {
				slog.Warn("app session: mint installation token failed",
					"installation", inst.ID, "account", inst.Account.Login, "error", err)
				continue
			}
			sctx := ghclient.WithToken(ctx, token)
			sctx = actor.WithActor(sctx, AppInstallationActor(inst.ID))
			sessions = append(sessions, sctx)
		}
		return sessions, nil
	}
}

// AppInstallationActor returns the stable cache-partition key for a GitHub App
// installation.
func AppInstallationActor(installID int64) string {
	return fmt.Sprintf("app-installation:%d", installID)
}
