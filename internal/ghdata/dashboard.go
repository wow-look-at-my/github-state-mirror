package ghdata

import (
	"context"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Dashboard / principal identities.
//
// The dashboard reports on the ONE global truth store (row counts, freshness)
// plus the reveal layer's principals: who has been seen, what they hold grants
// for, and how fresh their grant syncs are. It never bypasses admin gating in
// the API layer for the all-principals views.

// DataCounts is the global truth store's per-table row tally.
type DataCounts struct {
	Repos        int64 `json:"repos"`
	PullRequests int64 `json:"pull_requests"`
	CommitChecks int64 `json:"commit_checks"`
	Contents     int64 `json:"contents"`
	GitCommits   int64 `json:"git_commits"`
	Grants       int64 `json:"grants"`
}

// RecordActorIdentity remembers which GitHub login a principal authenticated
// as, updating last_seen on every call. The raw token is never passed in --
// only the principal key.
func (s *Store) RecordActorIdentity(ctx context.Context, principal, login string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.q.UpsertActorIdentity(ctx, dbgen.UpsertActorIdentityParams{
		Actor:     principal,
		Login:     login,
		FirstSeen: now,
		LastSeen:  now,
	})
}

// ListActorIdentities returns every known (principal -> login) mapping.
func (s *Store) ListActorIdentities(ctx context.Context) ([]dbgen.ActorIdentity, error) {
	return s.q.ListActorIdentities(ctx)
}

// KnownPrincipals returns every principal that holds freshness metadata, so
// the admin view can attribute even principals with no identity row (e.g. the
// background app-installation sessions).
func (s *Store) KnownPrincipals(ctx context.Context) ([]string, error) {
	return s.q.ListKnownPrincipals(ctx)
}

// GlobalDataCounts returns the truth store's per-table row counts.
func (s *Store) GlobalDataCounts(ctx context.Context) (DataCounts, error) {
	var c DataCounts
	var err error
	if c.Repos, err = s.q.CountRepos(ctx); err != nil {
		return c, err
	}
	if c.PullRequests, err = s.q.CountPullRequests(ctx); err != nil {
		return c, err
	}
	if c.CommitChecks, err = s.q.CountCommitChecks(ctx); err != nil {
		return c, err
	}
	if c.Contents, err = s.q.CountContentsCache(ctx); err != nil {
		return c, err
	}
	if c.GitCommits, err = s.q.CountGitCommitsCache(ctx); err != nil {
		return c, err
	}
	if c.Grants, err = s.q.CountAccessGrants(ctx); err != nil {
		return c, err
	}
	return c, nil
}

// GrantsByPrincipal returns every UNEXPIRED grant one principal holds —
// live access only, matching CountLiveGrants (an expired row awaiting the
// opportunistic prune is not access).
func (s *Store) GrantsByPrincipal(ctx context.Context, principal string, now time.Time) ([]dbgen.AccessGrant, error) {
	return s.q.ListGrantsByPrincipal(ctx, dbgen.ListGrantsByPrincipalParams{
		Principal: principal, ExpiresAt: rfc3339(now),
	})
}

// CountLiveGrants returns how many unexpired grants a principal holds.
func (s *Store) CountLiveGrants(ctx context.Context, principal string, now time.Time) (int64, error) {
	return s.q.CountGrantsByPrincipal(ctx, dbgen.CountGrantsByPrincipalParams{
		Principal: principal, ExpiresAt: rfc3339(now),
	})
}

// FreshnessByKind returns cache_metadata for one actor (a principal, or
// 'global' truth markers) grouped by resource kind and fetch state.
func (s *Store) FreshnessByKind(ctx context.Context, actorKey string) ([]dbgen.ActorFreshnessByKindRow, error) {
	return s.q.ActorFreshnessByKind(ctx, actorKey)
}

// ErrorMessagesByKind returns the captured failure reason for every resource
// currently in the error state for one actor, so the dashboard can show why a
// kind is erroring (not just that it is).
func (s *Store) ErrorMessagesByKind(ctx context.Context, actorKey string) ([]dbgen.ActorErrorMessagesByKindRow, error) {
	return s.q.ActorErrorMessagesByKind(ctx, actorKey)
}

// RecentRefreshes returns the most recent refresh-log entries for one actor.
func (s *Store) RecentRefreshes(ctx context.Context, actorKey string, limit int64) ([]dbgen.CacheRefreshLog, error) {
	return s.q.ActorRecentRefreshes(ctx, dbgen.ActorRecentRefreshesParams{Actor: actorKey, Limit: limit})
}

// WebhookDelivery is one recorded webhook delivery and what the dispatcher did
// with it. It is global -- see the webhook_deliveries table.
type WebhookDelivery struct {
	DeliveryID  string `json:"delivery_id"`
	EventType   string `json:"event_type"`
	Action      string `json:"action"`
	Repo        string `json:"repo"`
	ReceivedAt  string `json:"received_at"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

// webhookDeliveryKeep caps how many delivery-log rows are retained. The log is
// observability, not source-of-truth, so old rows are pruned on each insert.
const webhookDeliveryKeep = 500

// RecordWebhookDelivery appends a delivery to the global webhook log and prunes
// it back to the most recent webhookDeliveryKeep rows.
func (s *Store) RecordWebhookDelivery(ctx context.Context, d WebhookDelivery) error {
	receivedAt := d.ReceivedAt
	if receivedAt == "" {
		receivedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := s.q.InsertWebhookDelivery(ctx, dbgen.InsertWebhookDeliveryParams{
		DeliveryID:  d.DeliveryID,
		EventType:   d.EventType,
		Action:      d.Action,
		Repo:        d.Repo,
		ReceivedAt:  receivedAt,
		Disposition: d.Disposition,
		Detail:      d.Detail,
	}); err != nil {
		return err
	}
	return s.q.PruneWebhookDeliveries(ctx, webhookDeliveryKeep)
}

// RecentWebhookDeliveries returns the most recent webhook deliveries, newest first.
func (s *Store) RecentWebhookDeliveries(ctx context.Context, limit int64) ([]WebhookDelivery, error) {
	rows, err := s.q.ListRecentWebhookDeliveries(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]WebhookDelivery, len(rows))
	for i, r := range rows {
		out[i] = WebhookDelivery{
			DeliveryID:  r.DeliveryID,
			EventType:   r.EventType,
			Action:      r.Action,
			Repo:        r.Repo,
			ReceivedAt:  r.ReceivedAt,
			Disposition: r.Disposition,
			Detail:      r.Detail,
		}
	}
	return out, nil
}
