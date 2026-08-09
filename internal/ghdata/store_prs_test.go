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

// Store tests for the PR rows themselves: cascade deletes, the single-row
// reads, the mergeable/status COALESCE rules, labels, and row freshness.

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

// Ping is what the update-liveness probe asserts beyond "the process answered
// HTTP": a server still serving over a dead database is exactly the state a
// post-update health gate exists to catch, and it has to fail loudly there.
func TestPingFailsOnAClosedDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	store := NewStore(db)

	require.Nil(t, store.Ping(context.Background()))

	require.Nil(t, db.Close())
	assert.Error(t, store.Ping(context.Background()))
}
