package ghdata

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// A stale write must never resurrect a closed PR (see CLAUDE.md).

func openPR(number int64, updatedAt string) dbgen.PullRequest {
	return dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: number, Title: "PR", Url: "u",
		State: "OPEN", CreatedAt: "2026-08-10T15:00:00Z", UpdatedAt: updatedAt,
	}
}

func TestClosedPRIsNotResurrectedByAnOlderWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.UpsertPR(ctx, openPR(24, "2026-08-10T15:06:10Z"), now))
	require.NoError(t, s.DeletePR(ctx, "org1", "repo1", 24, "2026-08-10T15:08:18Z", now))

	// The failed:: delivery arrives late (or redelivered) after the close.
	require.NoError(t, s.UpsertPR(ctx, openPR(24, "2026-08-10T15:06:10Z"), now.Add(90*time.Second)))

	_, err := s.GetPullRequest(ctx, "org1", "repo1", 24)
	assert.Equal(t, sql.ErrNoRows, err, "a pre-close view must not re-open the PR")
}

func TestClosedPRIsReopenedByANewerWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.UpsertPR(ctx, openPR(24, "2026-08-10T15:06:10Z"), now))
	require.NoError(t, s.DeletePR(ctx, "org1", "repo1", 24, "2026-08-10T15:08:18Z", now))

	// A genuine reopen postdates the close, so it applies.
	require.NoError(t, s.UpsertPR(ctx, openPR(24, "2026-08-10T15:30:00Z"), now.Add(time.Hour)))

	got, err := s.GetPullRequest(ctx, "org1", "repo1", 24)
	require.NoError(t, err)
	assert.Equal(t, "2026-08-10T15:30:00Z", got.UpdatedAt)

	// The closure record cleared on reopen; the PR behaves like any open row.
	require.NoError(t, s.UpsertPR(ctx, openPR(24, "2026-08-10T15:31:00Z"), now.Add(time.Hour)))
	got, err = s.GetPullRequest(ctx, "org1", "repo1", 24)
	require.NoError(t, err)
	assert.Equal(t, "2026-08-10T15:31:00Z", got.UpdatedAt)
}

func TestPRWriteWithNoUpdatedAtCannotReopenAClosedPR(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.DeletePR(ctx, "org1", "repo1", 24, "2026-08-10T15:08:18Z", now))
	require.NoError(t, s.UpsertPR(ctx, openPR(24, ""), now))

	_, err := s.GetPullRequest(ctx, "org1", "repo1", 24)
	assert.Equal(t, sql.ErrNoRows, err, "a write that cannot prove it postdates the close is refused")
}

func TestPRClosureOnlyGuardsItsOwnPR(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.DeletePR(ctx, "org1", "repo1", 24, "2026-08-10T15:08:18Z", now))
	require.NoError(t, s.UpsertPR(ctx, openPR(25, "2026-08-10T15:06:10Z"), now))
	require.NoError(t, s.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo2", Number: 24, Title: "PR", Url: "u",
		State: "OPEN", CreatedAt: "2026-08-10T15:00:00Z", UpdatedAt: "2026-08-10T15:06:10Z",
	}, now))

	_, err := s.GetPullRequest(ctx, "org1", "repo1", 25)
	assert.NoError(t, err, "a sibling PR in the same repo is unaffected")
	_, err = s.GetPullRequest(ctx, "org1", "repo2", 24)
	assert.NoError(t, err, "the same number in another repo is unaffected")
}

func TestPRClosureFoldsRepoKeyCase(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.DeletePR(ctx, "Org1", "Repo1", 24, "2026-08-10T15:08:18Z", now))
	require.NoError(t, s.UpsertPR(ctx, openPR(24, "2026-08-10T15:06:10Z"), now))

	_, err := s.GetPullRequest(ctx, "org1", "repo1", 24)
	assert.Equal(t, sql.ErrNoRows, err, "the record is keyed lowercase, so casing cannot dodge it")
}

func TestPRClosureExpires(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	closed := time.Now().Add(-PRClosureRetention - time.Hour)

	require.NoError(t, s.DeletePR(ctx, "org1", "repo1", 24, "2026-08-10T15:08:18Z", closed))
	// Any later close prunes what has aged out.
	require.NoError(t, s.DeletePR(ctx, "org1", "repo1", 99, "2026-08-10T15:08:18Z", time.Now()))
	require.NoError(t, s.UpsertPR(ctx, openPR(24, "2026-08-10T15:06:10Z"), time.Now()))

	_, err := s.GetPullRequest(ctx, "org1", "repo1", 24)
	assert.NoError(t, err, "past the retention window nothing still holds a stale view of the PR")
}

func TestUpsertPRWithChecksRefusesAPreCloseView(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	_, err := s.ApplyCommitStatus(ctx, "org1", "repo1", "sha1", "ci", "SUCCESS", false)
	require.NoError(t, err)
	require.NoError(t, s.DeletePR(ctx, "org1", "repo1", 24, "2026-08-10T15:08:18Z", now))

	pr := openPR(24, "2026-08-10T15:06:10Z")
	pr.HeadRefOid = sql.NullString{String: "sha1", Valid: true}
	require.NoError(t, s.UpsertPRWithChecks(ctx, pr, []dbgen.PrLabel{
		{Owner: "org1", Repo: "repo1", PrNumber: 24, Name: "bug", Color: "red"},
	}, now))

	_, err = s.GetPullRequest(ctx, "org1", "repo1", 24)
	assert.Equal(t, sql.ErrNoRows, err)
	labels, err := s.ListPRLabels(ctx, "org1", "repo1", 24)
	require.NoError(t, err)
	assert.Empty(t, labels, "a refused write leaves no labels behind for a PR that is not there")
}

// A reconcile sweep infers the close from ABSENCE from an eventually
// consistent snapshot, and a wrong inference recorded as a closure would
// refuse the PR's real deliveries for a day. Only a statement that the PR
// closed records.
func TestSyncOrgTruthSweepRecordsNoClosure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.UpsertRepo(ctx, dbgen.Repo{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}))
	require.NoError(t, s.UpsertPR(ctx, openPR(24, "2026-08-10T15:06:10Z"), now.Add(-time.Hour)))

	// The snapshot no longer lists #, so the reconcile drops it.
	require.NoError(t, s.SyncOrgTruth(ctx, "org1", OrgSyncData{
		Repos:      []dbgen.Repo{{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u"}},
		PRsByRepo:  map[string][]dbgen.PullRequest{},
		LabelsByPR: map[string]map[int64][]dbgen.PrLabel{},
	}, "app:1", now, now))

	_, err := s.GetPullRequest(ctx, "org1", "repo1", 24)
	require.Equal(t, sql.ErrNoRows, err)

	require.NoError(t, s.UpsertPR(ctx, openPR(24, "2026-08-10T15:06:10Z"), now))
	_, err = s.GetPullRequest(ctx, "org1", "repo1", 24)
	assert.NoError(t, err, "a swept row comes back on its next delivery; the sweep re-runs if it was right")
}
