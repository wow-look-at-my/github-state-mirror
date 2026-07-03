package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
)

// SessionFunc yields the authenticated contexts to refresh on each cycle. Every
// returned context must carry a GitHub token (ghclient.WithToken) and a cache
// partition (actor.WithActor). It is called fresh each cycle so short-lived
// credentials (e.g. GitHub App installation tokens) can be re-minted. A nil
// SessionFunc, or one returning no contexts, disables periodic refreshing —
// per-request data still works via the caller's own token.
type SessionFunc func(ctx context.Context) ([]context.Context, error)

// PeriodicRefresher runs a background loop that refreshes all known resources
// for each session credential.
type PeriodicRefresher struct {
	mgr      *freshness.Manager
	interval time.Duration
	sessions SessionFunc
}

func NewPeriodicRefresher(mgr *freshness.Manager, interval time.Duration, sessions SessionFunc) *PeriodicRefresher {
	return &PeriodicRefresher{mgr: mgr, interval: interval, sessions: sessions}
}

// Start launches the periodic refresh loop. It blocks until ctx is canceled.
func (p *PeriodicRefresher) Start(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refreshAll(ctx)
		}
	}
}

func (p *PeriodicRefresher) refreshAll(ctx context.Context) {
	if p.sessions == nil {
		return
	}
	sessions, err := p.sessions(ctx)
	if err != nil {
		slog.Warn("periodic refresh: could not build sessions", "error", err)
		return
	}
	if len(sessions) == 0 {
		return
	}

	slog.Info("periodic refresh starting", "sessions", len(sessions))
	kinds := []string{KindOrgRepos}
	for _, sctx := range sessions {
		for _, kind := range kinds {
			if err := p.mgr.RefreshAllOfKind(sctx, kind, freshness.TriggerPeriodic); err != nil {
				slog.Warn("periodic refresh failed", "kind", kind, "error", err)
			}
		}
	}
	slog.Info("periodic refresh complete")
}
