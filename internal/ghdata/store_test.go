package ghdata

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)

	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func orgData(repos []dbgen.Repo, prsByRepo map[string][]dbgen.PullRequest) OrgSyncData {
	return OrgSyncData{Repos: repos, PRsByRepo: prsByRepo, LabelsByPR: map[string]map[int64][]dbgen.PrLabel{}}
}

// TestSyncOrgTruth_UpsertsNeverDeletesRepos locks the upsert-only repo
// reconcile: a fetch is one principal's PARTIAL view of the org (private repos
// they can't see, archived repos, ... are absent), so a repo missing from a
// later sync must SURVIVE. Deletion authority belongs to repository webhooks.
func TestSyncOrgTruth_UpsertsNeverDeletesRepos(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	repos := []dbgen.Repo{
		{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u1"},
		{Owner: "org1", Name: "repo2", NameWithOwner: "org1/repo2", Url: "u2"},
	}
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData(repos, nil), "user:1", now, now))

	got, err := s.ListReposByOwner(ctx, "org1")
	require.Nil(t, err)
	require.Equal(t, 2, len(got))

	// A later sync (a narrower principal) returning only repo1: repo2 survives.
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData(repos[:1], nil), "user:2", now, now))
	got, _ = s.ListReposByOwner(ctx, "org1")
	assert.Equal(t, 2, len(got), "a partial fetch must never delete repos from global truth")
}

// TestSyncOrgTruth_VisibilityPreserved: the GraphQL org fetch cannot carry
// visibility, so a sync's empty visibility must not clobber webhook-learned
// truth (in either direction).
func TestSyncOrgTruth_VisibilityPreserved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u",
		Visibility: VisibilityPublic,
	}))
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData([]dbgen.Repo{
		{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"},
	}, nil), "user:1", now, now))

	got, err := s.GetRepo(ctx, "org1", "repo1")
	require.NoError(t, err)
	assert.Equal(t, VisibilityPublic, got.Visibility, "an unknowing sync must not erase known visibility")
}

// TestSyncOrgTruth_StampsVisibility: a sync whose fetch DID carry visibility
// (the owner-agnostic query the fleet refresher uses) stamps it into truth --
// and a later visibility-less sync (the identity-locked org-query path) keeps
// it. This is the 2026-07-20 fix: the refresher used to sync visibility-less
// rows, leaving every fleet-synced owner's repo at '' = fail-closed unknown
// (203 visibility_unknown entries in that day's consistency report).
func TestSyncOrgTruth_StampsVisibility(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	// The owner-sync shape: the repo row carries the fetched visibility.
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData([]dbgen.Repo{
		{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u", Visibility: VisibilityPrivate},
	}, nil), "app-installation:1", now, now))

	got, err := s.GetRepo(ctx, "org1", "repo1")
	require.NoError(t, err)
	assert.Equal(t, VisibilityPrivate, got.Visibility, "an owner sync must stamp the visibility its fetch carried")

	// A later org-query-shaped sync (no visibility) must not blank it.
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData([]dbgen.Repo{
		{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"},
	}, nil), "user:1", now, now))
	got, err = s.GetRepo(ctx, "org1", "repo1")
	require.NoError(t, err)
	assert.Equal(t, VisibilityPrivate, got.Visibility, "a visibility-less sync must preserve the stamped value")

	// And a fresh owner sync reflecting a real change (privatized -> public)
	// still overwrites: the guard only blocks EMPTY values.
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData([]dbgen.Repo{
		{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u", Visibility: VisibilityPublic},
	}, nil), "app-installation:1", now, now))
	got, err = s.GetRepo(ctx, "org1", "repo1")
	require.NoError(t, err)
	assert.Equal(t, VisibilityPublic, got.Visibility, "a knowing sync must apply a changed visibility")
}

// TestSyncOrgTruth_ReconcilesOpenPRsWithGrace: for repos the fetch RETURNED,
// its open-PR list is authoritative -- stale open rows are deleted -- except
// rows touched inside the grace window (a webhook racing the fetch's eventual
// consistency must never be clobbered; this exact race was hit in prototyping).
func TestSyncOrgTruth_ReconcilesOpenPRsWithGrace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	mkPR := func(n int64, title string) dbgen.PullRequest {
		return dbgen.PullRequest{
			Owner: "org1", Repo: "repo1", Number: n, Title: title, Url: "u",
			State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-01",
		}
	}
	repos := []dbgen.Repo{{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}}

	// Truth holds PRs 1 (stale, closed upstream long ago -- its webhook was
	// missed) and 2. Backdate their touched_at beyond the grace window.
	require.NoError(t, s.UpsertPR(ctx, mkPR(1, "stale"), now.Add(-time.Hour)))
	require.NoError(t, s.UpsertPR(ctx, mkPR(2, "kept"), now.Add(-time.Hour)))
	// PR 3 was JUST webhook-applied -- inside the grace window.
	require.NoError(t, s.UpsertPR(ctx, mkPR(3, "racing webhook"), now))

	// The fetch snapshot (taken at fetchStart=now) contains only PR 2: GraphQL
	// eventual consistency hasn't seen PR 3 yet, and PR 1 closed upstream.
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData(repos, map[string][]dbgen.PullRequest{
		"org1/repo1": {mkPR(2, "kept")},
	}), "user:1", now, now))

	prs, err := s.ListOpenPRsByRepo(ctx, "org1", "repo1")
	require.NoError(t, err)
	numbers := make([]int64, 0, len(prs))
	for _, pr := range prs {
		numbers = append(numbers, pr.Number)
	}
	assert.Equal(t, []int64{2, 3}, numbers,
		"the stale row is reconciled away; the webhook-touched row survives the racing fetch")
}

// TestSyncOrgTruth_GrantsReplaceSynced: every repo a principal's fetch returned
// earns them a list_sync grant; absence from the next sync revokes it; probe
// grants survive a list replace (an archived repo is absent from org fetches
// but still probe-accessible).
func TestSyncOrgTruth_GrantsReplaceSynced(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	repos := []dbgen.Repo{
		{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u1"},
		{Owner: "org1", Name: "repo2", NameWithOwner: "org1/repo2", Url: "u2"},
	}
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData(repos, nil), "user:1", now, now))
	// A probe grant for an archived repo the org list never returns.
	require.NoError(t, s.RecordGrant(ctx, "user:1", "org1", "old-archive", GrantSourceProbe, now))

	for _, repo := range []string{"repo1", "repo2", "old-archive"} {
		ok, err := s.HasGrant(ctx, "user:1", "org1", repo, now)
		require.NoError(t, err)
		assert.True(t, ok, repo)
	}

	// Access to repo2 was revoked upstream: the next sync omits it.
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", orgData(repos[:1], nil), "user:1", now, now))
	ok, err := s.HasGrant(ctx, "user:1", "org1", "repo2", now)
	require.NoError(t, err)
	assert.False(t, ok, "absence from the principal's own sync revokes the list_sync grant")
	ok, _ = s.HasGrant(ctx, "user:1", "org1", "repo1", now)
	assert.True(t, ok)
	ok, _ = s.HasGrant(ctx, "user:1", "org1", "old-archive", now)
	assert.True(t, ok, "probe grants survive a list replace-sync")
}

// TestGrants_TTLExpiry: a grant past its expiry no longer reveals; re-earning
// it (a probe 2xx) renews the TTL.
func TestGrants_TTLExpiry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.RecordGrant(ctx, "user:1", "Org1", "Repo1", GrantSourceProbe, now.Add(-GrantTTL-time.Minute)))
	ok, err := s.HasGrant(ctx, "user:1", "org1", "repo1", now)
	require.NoError(t, err)
	assert.False(t, ok, "an expired grant must not reveal")

	require.NoError(t, s.RecordGrant(ctx, "user:1", "org1", "repo1", GrantSourceProbe, now))
	ok, _ = s.HasGrant(ctx, "user:1", "ORG1", "REPO1", now)
	assert.True(t, ok, "grants match case-insensitively via normalized keys")

	require.NoError(t, s.RevokeGrant(ctx, "user:1", "org1", "repo1"))
	ok, _ = s.HasGrant(ctx, "user:1", "org1", "repo1", now)
	assert.False(t, ok)
}

// TestDenyVerdicts: recorded per (principal, resource), expire on their short
// TTL, and are cleared when the principal earns a grant for the repo.
func TestDenyVerdicts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.RecordDenyVerdict(ctx, "user:1", "contents", "org1/repo1/x?ref=", "org1", "repo1", 404, "Not Found", now))
	v, ok, err := s.GetDenyVerdict(ctx, "user:1", "contents", "org1/repo1/x?ref=", now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 404, v.Status)
	assert.Equal(t, "Not Found", v.Message)

	// Another principal is unaffected.
	_, ok, err = s.GetDenyVerdict(ctx, "user:2", "contents", "org1/repo1/x?ref=", now)
	require.NoError(t, err)
	assert.False(t, ok)

	// Expiry.
	_, ok, _ = s.GetDenyVerdict(ctx, "user:1", "contents", "org1/repo1/x?ref=", now.Add(DenyTTL+time.Minute))
	assert.False(t, ok, "deny verdicts expire on their short TTL")

	// Earning a grant clears the principal's verdicts for the repo.
	require.NoError(t, s.RecordDenyVerdict(ctx, "user:1", "contents", "org1/repo1/x?ref=", "org1", "repo1", 404, "Not Found", now))
	require.NoError(t, s.RecordGrant(ctx, "user:1", "org1", "repo1", GrantSourceProbe, now))
	_, ok, _ = s.GetDenyVerdict(ctx, "user:1", "contents", "org1/repo1/x?ref=", now)
	assert.False(t, ok, "a fresh grant supersedes stale denials")
}

// TestPruneAccessControl: expired grant and deny rows are deleted, live ones
// survive.
func TestPruneAccessControl(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	seedGrant := func(repo string, expires time.Time) {
		require.NoError(t, s.q.UpsertAccessGrant(ctx, dbgen.UpsertAccessGrantParams{
			Principal: "user:1", Owner: "org1", Repo: repo,
			GrantedAt: rfc3339(now.Add(-48 * time.Hour)), ExpiresAt: rfc3339(expires),
			Source: GrantSourceProbe,
		}))
	}
	seedGrant("expired", now.Add(-time.Minute))
	seedGrant("live", now.Add(time.Hour))
	require.NoError(t, s.RecordDenyVerdict(ctx, "user:1", "contents", "k-expired", "org1", "r", 404, "nf", now.Add(-DenyTTL-time.Minute)))
	require.NoError(t, s.RecordDenyVerdict(ctx, "user:1", "contents", "k-live", "org1", "r", 404, "nf", now))

	require.NoError(t, s.PruneAccessControl(ctx, now))

	counts, err := s.GlobalDataCounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts.Grants, "the expired grant row is gone")
	// GetDenyVerdict filters expiry in Go, so read the raw rows to prove the
	// expired one was actually deleted.
	_, err = s.q.GetDenyVerdict(ctx, dbgen.GetDenyVerdictParams{Principal: "user:1", ResourceKind: "contents", ResourceKey: "k-expired"})
	assert.Equal(t, sql.ErrNoRows, err, "the expired deny row is gone")
	_, err = s.q.GetDenyVerdict(ctx, dbgen.GetDenyVerdictParams{Principal: "user:1", ResourceKind: "contents", ResourceKey: "k-live"})
	assert.NoError(t, err, "the live deny row survives")
}

// TestWritePathsPruneOpportunistically: RecordGrant and SyncOrgTruth sweep
// expired access-control rows as a best-effort side effect, throttled to at
// most once per pruneInterval — so expired rows never accumulate unboundedly
// yet the hot write path doesn't sweep on every call.
func TestWritePathsPruneOpportunistically(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	seedExpired := func(principal, repo string) {
		require.NoError(t, s.q.UpsertAccessGrant(ctx, dbgen.UpsertAccessGrantParams{
			Principal: principal, Owner: "org1", Repo: repo,
			GrantedAt: rfc3339(now.Add(-2 * GrantTTL)), ExpiresAt: rfc3339(now.Add(-GrantTTL)),
			Source: GrantSourceProbe,
		}))
	}
	rowExists := func(principal, repo string) bool {
		_, err := s.q.GetAccessGrant(ctx, dbgen.GetAccessGrantParams{Principal: principal, Owner: "org1", Repo: repo})
		if err == sql.ErrNoRows {
			return false
		}
		require.NoError(t, err)
		return true
	}

	// The first write-path call prunes (nothing has throttled yet).
	seedExpired("user:2", "stale1")
	require.NoError(t, s.RecordGrant(ctx, "user:1", "org1", "fresh1", GrantSourceProbe, now))
	assert.False(t, rowExists("user:2", "stale1"), "RecordGrant sweeps the expired row")
	assert.True(t, rowExists("user:1", "fresh1"))

	// Inside the throttle window a later write does NOT sweep again.
	seedExpired("user:2", "stale2")
	require.NoError(t, s.RecordGrant(ctx, "user:1", "org1", "fresh2", GrantSourceProbe, now.Add(time.Minute)))
	assert.True(t, rowExists("user:2", "stale2"), "inside the throttle window the expired row stays")

	// Past the throttle window, SyncOrgTruth's write path sweeps it.
	later := now.Add(pruneInterval + time.Minute)
	require.NoError(t, s.SyncOrgTruth(ctx, "org1",
		orgData([]dbgen.Repo{{Owner: "org1", Name: "r1", NameWithOwner: "org1/r1", Url: "u"}}, nil),
		"user:1", later, later))
	assert.False(t, rowExists("user:2", "stale2"), "past the throttle window SyncOrgTruth sweeps the expired row")
	assert.True(t, rowExists("user:1", "fresh1"), "live rows survive the sweep")
}

// TestGrantsByPrincipal_OnlyUnexpired: the grants view lists LIVE access only,
// agreeing with CountLiveGrants (an expired row awaiting the opportunistic
// prune is not access).
func TestGrantsByPrincipal_OnlyUnexpired(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.RecordGrant(ctx, "user:1", "org1", "live", GrantSourceProbe, now))
	// Seeded raw (not via RecordGrant) so the write-path prune can't race the
	// expired row out before the assertion.
	require.NoError(t, s.q.UpsertAccessGrant(ctx, dbgen.UpsertAccessGrantParams{
		Principal: "user:1", Owner: "org1", Repo: "expired",
		GrantedAt: rfc3339(now.Add(-2 * GrantTTL)), ExpiresAt: rfc3339(now.Add(-time.Minute)),
		Source: GrantSourceProbe,
	}))

	rows, err := s.GrantsByPrincipal(ctx, "user:1", now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "live", rows[0].Repo)

	n, err := s.CountLiveGrants(ctx, "user:1", now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "list and count agree on live grants")
}

// TestDeleteRepoCascade removes the repo and everything hanging off it.
func TestDeleteRepoCascade(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.UpsertRepo(ctx, dbgen.Repo{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}))
	require.NoError(t, s.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 1, Title: "PR", Url: "u",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-01",
	}, now))
	require.NoError(t, s.SetPRLabels(ctx, "org1", "repo1", 1, []dbgen.PrLabel{{Owner: "org1", Repo: "repo1", PrNumber: 1, Name: "bug", Color: "red"}}))
	_, err := s.ApplyCommitStatus(ctx, "org1", "repo1", "sha1", "ci", "SUCCESS", false)
	require.NoError(t, err)
	require.NoError(t, s.RecordGrant(ctx, "user:1", "org1", "repo1", GrantSourceProbe, now))

	require.NoError(t, s.DeleteRepoCascade(ctx, "org1", "repo1"))

	_, err = s.GetRepo(ctx, "org1", "repo1")
	assert.Equal(t, sql.ErrNoRows, err)
	prs, _ := s.ListOpenPRsByRepo(ctx, "org1", "repo1")
	assert.Empty(t, prs)
	labels, _ := s.ListPRLabels(ctx, "org1", "repo1", 1)
	assert.Empty(t, labels)
	ok, _ := s.HasGrant(ctx, "user:1", "org1", "repo1", now)
	assert.False(t, ok, "grants for a deleted repo are gone")
}

func TestGetRepo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertRepo(ctx, dbgen.Repo{Owner: "Org1", Name: "Repo1", NameWithOwner: "Org1/Repo1", Url: "u1"}))

	got, err := s.GetRepo(ctx, "Org1", "Repo1")
	require.Nil(t, err)
	assert.Equal(t, "Org1/Repo1", got.NameWithOwner)

	// URL-cased lookups fold case.
	got, err = s.GetRepoInsensitive(ctx, "org1", "repo1")
	require.Nil(t, err)
	assert.Equal(t, "Org1/Repo1", got.NameWithOwner)
}

func TestGetPullRequest(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	pr := dbgen.PullRequest{Owner: "org1", Repo: "repo1", Number: 1, Title: "PR 1", Url: "u1", State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-01"}
	require.NoError(t, s.UpsertPR(ctx, pr, time.Now()))

	got, err := s.GetPullRequest(ctx, "org1", "repo1", 1)
	require.Nil(t, err)
	assert.Equal(t, "PR 1", got.Title)
}

// TestUpsertPR_MergeableNullDoesNotClobber locks the COALESCE on mergeable: a
// pull_request webhook that arrives while GitHub is still computing
// mergeability carries mergeable=null (and GitHub never re-delivers the event
// when it resolves), so a NULL in the payload must preserve a previously-known
// value — while a genuinely resolved value must still overwrite.
func TestUpsertPR_MergeableNullDoesNotClobber(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	base := dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 7, Title: "PR 7", Url: "u",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-01",
	}

	// Known value (from a GraphQL refresh or an earlier resolved webhook).
	pr := base
	pr.Mergeable = sql.NullString{String: "MERGEABLE", Valid: true}
	require.NoError(t, s.UpsertPR(ctx, pr, now))

	// Webhook payload while GitHub is computing mergeability: mergeable is null.
	pr = base
	pr.Mergeable = sql.NullString{} // NULL
	require.NoError(t, s.UpsertPR(ctx, pr, now))

	got, err := s.GetPullRequest(ctx, "org1", "repo1", 7)
	require.NoError(t, err)
	assert.True(t, got.Mergeable.Valid, "NULL mergeable in a webhook payload must not clobber the known value")
	assert.Equal(t, "MERGEABLE", got.Mergeable.String)

	// A genuinely resolved CONFLICTING must still overwrite.
	pr = base
	pr.Mergeable = sql.NullString{String: "CONFLICTING", Valid: true}
	require.NoError(t, s.UpsertPR(ctx, pr, now))

	got, err = s.GetPullRequest(ctx, "org1", "repo1", 7)
	require.NoError(t, err)
	assert.Equal(t, "CONFLICTING", got.Mergeable.String, "a resolved mergeable value must overwrite")
}

// TestUpsertPRWithChecks_DerivesStatusFromExistingChecks locks the on-upsert
// rollup: a PR opened AFTER its head commit's CI finished (a pr-minder
// auto-opened PR) arrives via webhook with no CI state, and no later check event
// will re-fire for that sha — so the upsert itself must derive
// last_commit_status from the commit checks already recorded for the head sha.
func TestUpsertPRWithChecks_DerivesStatusFromExistingChecks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	// CI finished for commit shaX before any PR existed (the check-event apply path).
	_, err := s.ApplyCommitStatus(ctx, "org1", "repo1", "shaX", "check_run:build", "SUCCESS", false)
	require.NoError(t, err)
	_, err = s.ApplyCommitStatus(ctx, "org1", "repo1", "shaX", "status:lint", "SUCCESS", false)
	require.NoError(t, err)

	// A PR opened afterwards with that head commit; the payload carries no CI state.
	pr := dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 5, Title: "late PR", Url: "u",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-01",
		HeadRefOid: sql.NullString{String: "shaX", Valid: true},
	}
	require.NoError(t, s.UpsertPRWithChecks(ctx, pr, nil, now))

	got, err := s.GetPullRequest(ctx, "org1", "repo1", 5)
	require.NoError(t, err)
	assert.True(t, got.LastCommitStatus.Valid, "last_commit_status must be derived from existing checks")
	assert.Equal(t, "SUCCESS", got.LastCommitStatus.String)

	// A failing check among the recorded states dominates the rollup.
	_, err = s.ApplyCommitStatus(ctx, "org1", "repo1", "shaY", "check_run:build", "FAILURE", false)
	require.NoError(t, err)
	pr.Number = 6
	pr.HeadRefOid = sql.NullString{String: "shaY", Valid: true}
	require.NoError(t, s.UpsertPRWithChecks(ctx, pr, nil, now))

	got, err = s.GetPullRequest(ctx, "org1", "repo1", 6)
	require.NoError(t, err)
	assert.Equal(t, "FAILURE", got.LastCommitStatus.String)
}

// TestUpsertPRWithChecks_NoChecksLeavesStatusNull is the counterpart: with no
// recorded checks for the head sha there is nothing to derive, and the upsert
// must not stomp the (COALESCE-preserved) status with an empty rollup.
func TestUpsertPRWithChecks_NoChecksLeavesStatusNull(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	pr := dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 8, Title: "no CI yet", Url: "u",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-01",
		HeadRefOid: sql.NullString{String: "shaNoChecks", Valid: true},
	}
	require.NoError(t, s.UpsertPRWithChecks(ctx, pr, nil, time.Now()))

	got, err := s.GetPullRequest(ctx, "org1", "repo1", 8)
	require.NoError(t, err)
	assert.False(t, got.LastCommitStatus.Valid, "no checks recorded: status must stay NULL")
}

func TestSetPRLabels(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Set initial labels.
	labels := []dbgen.PrLabel{
		{Owner: "org1", Repo: "repo1", PrNumber: 1, Name: "bug", Color: "red"},
		{Owner: "org1", Repo: "repo1", PrNumber: 1, Name: "urgent", Color: "orange"},
	}
	require.NoError(t, s.SetPRLabels(ctx, "org1", "repo1", 1, labels))

	got, err := s.ListPRLabels(ctx, "org1", "repo1", 1)
	require.Nil(t, err)
	require.Equal(t, 2, len(got))

	// Replace with different labels.
	newLabels := []dbgen.PrLabel{
		{Owner: "org1", Repo: "repo1", PrNumber: 1, Name: "enhancement", Color: "blue"},
	}
	require.NoError(t, s.SetPRLabels(ctx, "org1", "repo1", 1, newLabels))

	got, err = s.ListPRLabels(ctx, "org1", "repo1", 1)
	require.Nil(t, err)
	require.Equal(t, 1, len(got))
	assert.Equal(t, "enhancement", got[0].Name)
}

// TestPRRowFresh covers the single-PR staleness backstop predicate.
func TestPRRowFresh(t *testing.T) {
	now := time.Now()
	fresh := dbgen.PullRequest{TouchedAt: now.Add(-time.Hour).UTC().Format(time.RFC3339)}
	stale := dbgen.PullRequest{TouchedAt: now.Add(-PRRowTTL - time.Hour).UTC().Format(time.RFC3339)}
	assert.True(t, PRRowFresh(fresh, now))
	assert.False(t, PRRowFresh(stale, now), "a row untouched past PRRowTTL is stale")
	assert.False(t, PRRowFresh(dbgen.PullRequest{}, now), "an empty touched_at is stale (fail to a re-fetch)")
	assert.False(t, PRRowFresh(dbgen.PullRequest{TouchedAt: "garbage"}, now))
}
