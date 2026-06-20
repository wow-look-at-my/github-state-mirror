package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
)

// Session is one credential partition to refresh on a cycle. Ctx must carry a
// GitHub token (ghclient.WithToken) and a cache partition (actor.WithActor).
// Orgs lists the org logins whose repos this credential can see; the refresher
// proactively populates org-repos for each, so a partition that has never been
// touched by a request (e.g. a freshly-minted app-installation bucket) still
// gets warmed. This is what gives the webhook dispatcher a durable scope to
// apply to instead of skipping with "no cached scope".
type Session struct {
	Ctx  context.Context
	Orgs []string
}

// SessionFunc yields the sessions to refresh on each cycle. It is called fresh
// each cycle so short-lived credentials (e.g. GitHub App installation tokens)
// can be re-minted. A nil SessionFunc, or one returning no sessions, disables
// periodic refreshing — per-request data still works via the caller's own token.
type SessionFunc func(ctx context.Context) ([]Session, error)

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

// Start launches the periodic refresh loop. It runs one refresh immediately so a
// cold cache (e.g. just after a schema-bump nuke or a redeploy) warms up at once
// rather than after a full interval, then refreshes on every tick. It blocks
// until ctx is canceled.
func (p *PeriodicRefresher) Start(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	p.refreshAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refreshAll(ctx)
		}
	}
}

// backstopKinds are re-fetched for any resource already known in a session's
// partition. org_repos is intentionally excluded: it is handled by the
// per-session EnsureFresh sweep below (which also seeds it when unknown), so
// listing it here would double-fetch every cycle.
var backstopKinds = []string{KindUser, KindUserOrgs, KindPRFiles, KindCompare}

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
	for _, s := range sessions {
		// Proactively populate org-repos for every org this credential covers.
		// EnsureFresh creates+fetches the resource when the partition has never
		// seen it, so a cold app-installation bucket gets fully warmed here, and
		// re-fetches it once the TTL expires on later cycles (the documented
		// 6-hourly backstop). This is what lets the webhook dispatcher find a
		// durable actor for the repo and apply instead of skipping.
		for _, org := range s.Orgs {
			id := freshness.ResourceID{Kind: KindOrgRepos, Key: org}
			if err := p.mgr.EnsureFresh(s.Ctx, id); err != nil {
				slog.Warn("periodic seed org repos failed", "org", org, "error", err)
			}
		}
		// Backstop: re-fetch anything else already cached in this partition.
		for _, kind := range backstopKinds {
			if err := p.mgr.RefreshAllOfKind(s.Ctx, kind, freshness.TriggerPeriodic); err != nil {
				slog.Warn("periodic refresh failed", "kind", kind, "error", err)
			}
		}
	}
	slog.Info("periodic refresh complete")
}
