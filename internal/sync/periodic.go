package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
)

// PeriodicRefresher runs a background loop that refreshes all known resources.
type PeriodicRefresher struct {
	mgr      *freshness.Manager
	interval time.Duration
}

func NewPeriodicRefresher(mgr *freshness.Manager, interval time.Duration) *PeriodicRefresher {
	return &PeriodicRefresher{mgr: mgr, interval: interval}
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
	slog.Info("periodic refresh starting")
	kinds := []string{KindUser, KindUserOrgs, KindOrgRepos}
	for _, kind := range kinds {
		if err := p.mgr.RefreshAllOfKind(ctx, kind, freshness.TriggerPeriodic); err != nil {
			slog.Warn("periodic refresh failed", "kind", kind, "error", err)
		}
	}
	slog.Info("periodic refresh complete")
}
