package ghdata

import (
	"context"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Dashboard / Actor Identities.
//
// These operate across actors (or by GitHub login) rather than reading the
// actor from context, because the dashboard aggregates a user's own scopes and,
// for an admin, every scope. They never expose one credential's cached rows to
// another; they only surface counts and freshness metadata.

// DataCounts is the per-table cached-row tally for a single actor (cache scope).
type DataCounts struct {
	Repos             int64 `json:"repos"`
	PullRequests      int64 `json:"pull_requests"`
	Orgs              int64 `json:"orgs"`
	Users             int64 `json:"users"`
	CommitChecks      int64 `json:"commit_checks"`
	PRFiles           int64 `json:"pr_files"`
	BranchComparisons int64 `json:"branch_comparisons"`
}

// RecordActorIdentity remembers which GitHub login a token fingerprint (actor)
// authenticated as, updating last_seen on every call. The raw token is never
// passed in — only its fingerprint.
func (s *Store) RecordActorIdentity(ctx context.Context, actorFP, login string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.q.UpsertActorIdentity(ctx, dbgen.UpsertActorIdentityParams{
		Actor:     actorFP,
		Login:     login,
		FirstSeen: now,
		LastSeen:  now,
	})
}

// ListActorIdentities returns every known (actor -> login) mapping.
func (s *Store) ListActorIdentities(ctx context.Context) ([]dbgen.ActorIdentity, error) {
	return s.q.ListActorIdentities(ctx)
}

// CachedActors returns every distinct actor that has any cache metadata, so the
// admin view can attribute scopes that lack an identity row.
func (s *Store) CachedActors(ctx context.Context) ([]string, error) {
	return s.q.ListCachedActors(ctx)
}

// DataCounts returns the per-table cached-row counts for one actor.
func (s *Store) DataCounts(ctx context.Context, actorFP string) (DataCounts, error) {
	var c DataCounts
	var err error
	if c.Repos, err = s.q.CountReposByActor(ctx, actorFP); err != nil {
		return c, err
	}
	if c.PullRequests, err = s.q.CountPullRequestsByActor(ctx, actorFP); err != nil {
		return c, err
	}
	if c.Orgs, err = s.q.CountOrgsByActor(ctx, actorFP); err != nil {
		return c, err
	}
	if c.Users, err = s.q.CountUsersByActor(ctx, actorFP); err != nil {
		return c, err
	}
	if c.CommitChecks, err = s.q.CountCommitChecksByActor(ctx, actorFP); err != nil {
		return c, err
	}
	if c.PRFiles, err = s.q.CountPRFilesByActor(ctx, actorFP); err != nil {
		return c, err
	}
	if c.BranchComparisons, err = s.q.CountBranchComparisonsByActor(ctx, actorFP); err != nil {
		return c, err
	}
	return c, nil
}

// FreshnessByKind returns cache_metadata for one actor grouped by resource kind
// and fetch state.
func (s *Store) FreshnessByKind(ctx context.Context, actorFP string) ([]dbgen.ActorFreshnessByKindRow, error) {
	return s.q.ActorFreshnessByKind(ctx, actorFP)
}

// RecentRefreshes returns the most recent refresh-log entries for one actor.
func (s *Store) RecentRefreshes(ctx context.Context, actorFP string, limit int64) ([]dbgen.CacheRefreshLog, error) {
	return s.q.ActorRecentRefreshes(ctx, dbgen.ActorRecentRefreshesParams{Actor: actorFP, Limit: limit})
}

// WebhookDelivery is one recorded webhook delivery and what the dispatcher did
// with it. It is global (not actor-scoped) — see the webhook_deliveries table.
type WebhookDelivery struct {
	DeliveryID  string `json:"delivery_id"`
	EventType   string `json:"event_type"`
	Action      string `json:"action"`
	Repo        string `json:"repo"`
	ReceivedAt  string `json:"received_at"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
	Actors      int64  `json:"actors"`
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
		Actors:      d.Actors,
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
			Actors:      r.Actors,
		}
	}
	return out, nil
}
