package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
)

// PeriodicRefresher runs a background loop that refreshes each installation
// session's OWNER — the fleet sync. It names the resource to fetch from the
// session itself (owner = the installation account), so a fresh installation
// with no pre-existing freshness row is synced on the very first cycle.
//
// The old shape — RefreshAllOfKind over the actor's KNOWN resources — was a
// production no-op: a cache_metadata row is only ever created inside doFetch,
// which RefreshAllOfKind only reaches for rows that already exist, and nothing
// else ever wrote a row under an app-installation actor (chicken-and-egg).
type PeriodicRefresher struct {
	mgr      *freshness.Manager
	interval time.Duration
	sessions SessionFunc
}

func NewPeriodicRefresher(mgr *freshness.Manager, interval time.Duration, sessions SessionFunc) *PeriodicRefresher {
	return &PeriodicRefresher{mgr: mgr, interval: interval, sessions: sessions}
}

// Start launches the periodic refresh loop: one fleet refresh immediately at
// startup, then one per interval. It blocks until ctx is canceled.
//
// The startup run is load-bearing: a bare ticker's first fire is a full
// interval after process start, and under a deploy cadence shorter than the
// interval (schema-bump deploys also nuke the freshness markers) the fleet
// sync never completed at all.
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
		// InvalidateAndRefresh reaches doFetch directly: it creates the missing
		// cache_metadata row itself (killing the seed-first requirement) and
		// TriggerPeriodic bypasses the lazy error-backoff, so a deliberate
		// refresh always actually fetches.
		id := freshness.ResourceID{Kind: KindOrgRepos, Key: s.Owner}
		if err := p.mgr.InvalidateAndRefresh(s.Ctx, id, freshness.TriggerPeriodic); err != nil {
			slog.Warn("periodic refresh failed", "owner", s.Owner, "installation", s.InstallationID, "error", err)
		}
	}
	slog.Info("periodic refresh complete")
}
