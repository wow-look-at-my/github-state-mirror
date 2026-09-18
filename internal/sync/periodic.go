package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
)

// PeriodicRefresher runs the fleet sync: each installation session's owner.
// see docs/reveal-layer.md
type PeriodicRefresher struct {
	mgr      *freshness.Manager
	interval time.Duration
	sessions SessionFunc
}

func NewPeriodicRefresher(mgr *freshness.Manager, interval time.Duration, sessions SessionFunc) *PeriodicRefresher {
	return &PeriodicRefresher{mgr: mgr, interval: interval, sessions: sessions}
}

// Start runs fleet refresh immediately, then per interval, until ctx
// is canceled. The immediate run is load-bearing: see docs/reveal-layer.md
func (p *PeriodicRefresher) Start(ctx context.Context) {
	if ctx.Err() == nil {
		p.refreshAll(ctx)
	}

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
	for i, s := range sessions {
		// A shutdown mid-cycle must be visible, not a silently truncated
		// fleet: stop starting new fetches and say how much was left undone.
		if ctx.Err() != nil {
			slog.Warn("periodic refresh interrupted by shutdown", "owners_remaining", len(sessions)-i)
			return
		}
		if s.Owner == "" {
			continue
		}
		// Seeds the freshness row itself and bypasses the lazy error-backoff.
		id := freshness.ResourceID{Kind: KindOrgRepos, Key: s.Owner}
		if err := p.mgr.InvalidateAndRefresh(s.Ctx, id, freshness.TriggerPeriodic); err != nil {
			slog.Warn("periodic refresh failed", "owner", s.Owner, "installation", s.InstallationID, "error", err)
		}
	}
	slog.Info("periodic refresh complete")
}
