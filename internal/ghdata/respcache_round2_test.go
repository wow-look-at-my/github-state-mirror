package ghdata

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherTestSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestCachedWorkflowRuns_RoundTrip: put/get one workflow-runs page per exact
// pagination shape, expiry as a miss, per-sha and repo-wide invalidation.
func TestCachedWorkflowRuns_RoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.PutCachedWorkflowRuns(ctx, "Org1", "Repo1", testSHA, 30, 1, `{"total_count":1}`, now, time.Hour))
	require.NoError(t, s.PutCachedWorkflowRuns(ctx, "org1", "repo1", testSHA, 50, 2, `{"total_count":2}`, now, time.Hour))
	require.NoError(t, s.PutCachedWorkflowRuns(ctx, "org1", "repo1", otherTestSHA, 30, 1, `{"total_count":3}`, now, time.Hour))

	// Keys normalize (URL casing folds) and the pagination shape is part of
	// the key: each shape is its own row.
	doc, ok, err := s.GetCachedWorkflowRuns(ctx, "ORG1", "REPO1", testSHA, 30, 1, now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `{"total_count":1}`, doc)
	doc, ok, err = s.GetCachedWorkflowRuns(ctx, "org1", "repo1", testSHA, 50, 2, now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `{"total_count":2}`, doc)
	_, ok, err = s.GetCachedWorkflowRuns(ctx, "org1", "repo1", testSHA, 100, 1, now)
	require.NoError(t, err)
	assert.False(t, ok, "an unstored pagination shape must miss")

	// An expired row is a miss.
	_, ok, err = s.GetCachedWorkflowRuns(ctx, "org1", "repo1", testSHA, 30, 1, now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.False(t, ok, "an expired snapshot must miss")

	// Per-sha invalidation drops both of the sha's pages, keeps the other sha.
	require.NoError(t, s.InvalidateWorkflowRunsForHeadSHA(ctx, "org1", "repo1", testSHA))
	_, ok, err = s.GetCachedWorkflowRuns(ctx, "org1", "repo1", testSHA, 50, 2, now)
	require.NoError(t, err)
	assert.False(t, ok, "the sha's pages must be gone")
	_, ok, err = s.GetCachedWorkflowRuns(ctx, "org1", "repo1", otherTestSHA, 30, 1, now)
	require.NoError(t, err)
	assert.True(t, ok, "another sha's pages must survive a per-sha flush")

	// Repo-wide invalidation drops the rest.
	require.NoError(t, s.InvalidateWorkflowRunsCache(ctx, "org1", "repo1"))
	_, ok, err = s.GetCachedWorkflowRuns(ctx, "org1", "repo1", otherTestSHA, 30, 1, now)
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestCachedGitCommitMiss_RoundTrip: put/get one 404 verdict, expiry as a
// miss, the explicit clear, and the repo-wide flush.
func TestCachedGitCommitMiss_RoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.PutCachedGitCommitMiss(ctx, "Org1", "Repo1", testSHA, `{"message":"Not Found"}`, now, time.Hour))

	doc, ok, err := s.GetCachedGitCommitMiss(ctx, "org1", "repo1", testSHA, now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `{"message":"Not Found"}`, doc)

	_, ok, err = s.GetCachedGitCommitMiss(ctx, "org1", "repo1", testSHA, now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.False(t, ok, "an expired miss marker must not answer")

	require.NoError(t, s.ClearGitCommitMiss(ctx, "org1", "repo1", testSHA))
	_, ok, err = s.GetCachedGitCommitMiss(ctx, "org1", "repo1", testSHA, now)
	require.NoError(t, err)
	assert.False(t, ok, "a cleared miss marker must not answer")

	require.NoError(t, s.PutCachedGitCommitMiss(ctx, "org1", "repo1", testSHA, `{"message":"Not Found"}`, now, time.Hour))
	require.NoError(t, s.InvalidateGitCommitMissCache(ctx, "org1", "repo1"))
	_, ok, err = s.GetCachedGitCommitMiss(ctx, "org1", "repo1", testSHA, now)
	require.NoError(t, err)
	assert.False(t, ok, "a repo-wide flush must drop miss markers")
}

// TestGitCommitUpsert_ClearsMissMarker locks the round-2 invariant: EVERY
// path that upserts a REAL git commit funnels through ghdata.upsertGitCommit,
// which clears the sha's 404 miss marker -- so a sha that materializes stops
// answering 404 immediately, no matter which absorber saw it first.
func TestGitCommitUpsert_ClearsMissMarker(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	commit := func(sha string) CachedGitCommit {
		return CachedGitCommit{
			Owner: "org1", Repo: "repo1", SHA: sha,
			TreeSHA: "1111111111111111111111111111111111111111", Message: "m",
		}
	}
	missGone := func(t *testing.T, s *Store, sha string) {
		t.Helper()
		_, ok, err := s.GetCachedGitCommitMiss(ctx, "org1", "repo1", sha, now)
		require.NoError(t, err)
		assert.False(t, ok, "the miss marker must be cleared by the real-commit upsert")
	}
	seedMiss := func(t *testing.T, s *Store, sha string) {
		t.Helper()
		require.NoError(t, s.PutCachedGitCommitMiss(ctx, "org1", "repo1", sha, `{"message":"Not Found"}`, now, time.Hour))
		_, ok, err := s.GetCachedGitCommitMiss(ctx, "org1", "repo1", sha, now)
		require.NoError(t, err)
		require.True(t, ok, "seeded miss marker must serve")
	}

	t.Run("single-commit put", func(t *testing.T) {
		s := testStore(t)
		seedMiss(t, s, testSHA)
		require.NoError(t, s.PutCachedGitCommit(ctx, commit(testSHA), now))
		missGone(t, s, testSHA)
	})

	t.Run("push-payload batch upsert", func(t *testing.T) {
		s := testStore(t)
		seedMiss(t, s, testSHA)
		require.NoError(t, s.UpsertGitCommits(ctx, []CachedGitCommit{commit(testSHA)}, now))
		missGone(t, s, testSHA)
	})

	t.Run("commits-list absorb", func(t *testing.T) {
		s := testStore(t)
		seedMiss(t, s, testSHA)
		require.NoError(t, s.PutCachedCommitsList(ctx, "org1", "repo1", "main", 30, 1,
			[]CachedGitCommit{commit(testSHA)}, now, time.Hour))
		missGone(t, s, testSHA)
	})

	t.Run("compare absorb", func(t *testing.T) {
		s := testStore(t)
		seedMiss(t, s, testSHA)
		require.NoError(t, s.PutCachedCompare(ctx, CachedCompare{
			Owner: "org1", Repo: "repo1", Basehead: "main...dev",
			BaseRef: "main", HeadRef: "dev", Status: 200, Doc: `{"status":"ahead"}`,
		}, []CachedGitCommit{commit(testSHA)}, now, time.Hour))
		missGone(t, s, testSHA)
	})
}

// TestCachedPullDiff406_RoundTrip: put/get one 406 verdict, expiry as a miss,
// per-PR and repo-wide invalidation.
func TestCachedPullDiff406_RoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.PutCachedPullDiff406(ctx, "Org1", "Repo1", 42, `{"message":"diff too large"}`, now, time.Hour))
	require.NoError(t, s.PutCachedPullDiff406(ctx, "org1", "repo1", 7, `{"message":"diff too large"}`, now, time.Hour))

	doc, ok, err := s.GetCachedPullDiff406(ctx, "org1", "repo1", 42, now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `{"message":"diff too large"}`, doc)

	_, ok, err = s.GetCachedPullDiff406(ctx, "org1", "repo1", 42, now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.False(t, ok, "an expired verdict must not answer")

	require.NoError(t, s.InvalidatePullDiff406ForPR(ctx, "org1", "repo1", 42))
	_, ok, err = s.GetCachedPullDiff406(ctx, "org1", "repo1", 42, now)
	require.NoError(t, err)
	assert.False(t, ok, "the PR's verdict must be gone")
	_, ok, err = s.GetCachedPullDiff406(ctx, "org1", "repo1", 7, now)
	require.NoError(t, err)
	assert.True(t, ok, "another PR's verdict must survive a per-PR flush")

	require.NoError(t, s.InvalidatePullDiff406Cache(ctx, "org1", "repo1"))
	_, ok, err = s.GetCachedPullDiff406(ctx, "org1", "repo1", 7, now)
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestInvalidateCommitCIForRef: the per-ref flush drops one verbatim ref
// spelling's snapshots across every kind and pagination shape, leaving other
// refs' snapshots alone; the pagination shape is part of the read key.
func TestInvalidateCommitCIForRef(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	put := func(ref, kind string, perPage, page int) {
		t.Helper()
		require.NoError(t, s.PutCachedCommitCI(ctx, CachedCommitCI{
			Owner: "org1", Repo: "repo1", Ref: ref, Kind: kind, Doc: `{"seeded":true}`,
		}, perPage, page, now, time.Hour))
	}
	serves := func(ref, kind string, perPage, page int) bool {
		t.Helper()
		_, ok, err := s.GetCachedCommitCI(ctx, "org1", "repo1", ref, kind, perPage, page, now)
		require.NoError(t, err)
		return ok
	}

	put("main", CommitCIKindStatus, 30, 1)
	put("main", CommitCIKindCheckRuns, 30, 1)
	put("main", CommitCIKindStatusesList, 50, 2)
	put("claude/dev", CommitCIKindStatus, 30, 1)

	// Pagination is part of the key.
	require.True(t, serves("main", CommitCIKindStatusesList, 50, 2))
	require.False(t, serves("main", CommitCIKindStatusesList, 30, 1))

	require.NoError(t, s.InvalidateCommitCIForRef(ctx, "org1", "repo1", "main"))
	assert.False(t, serves("main", CommitCIKindStatus, 30, 1), "the ref's status snapshot must be gone")
	assert.False(t, serves("main", CommitCIKindCheckRuns, 30, 1), "the ref's check-runs snapshot must be gone")
	assert.False(t, serves("main", CommitCIKindStatusesList, 50, 2), "the ref's statuses-list snapshot must be gone")
	assert.True(t, serves("claude/dev", CommitCIKindStatus, 30, 1), "another ref's snapshot must survive")
}

// TestInvalidateCompareForRef: the per-ref flush matches the ref on EITHER
// side of the stored basehead and leaves unrelated comparisons alone; the
// stored status round-trips through Get.
func TestInvalidateCompareForRef(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	put := func(basehead, baseRef, headRef string) {
		t.Helper()
		require.NoError(t, s.PutCachedCompare(ctx, CachedCompare{
			Owner: "org1", Repo: "repo1", Basehead: basehead,
			BaseRef: baseRef, HeadRef: headRef, Status: 200, Doc: `{"status":"ahead"}`,
		}, nil, now, time.Hour))
	}
	serves := func(basehead string) bool {
		t.Helper()
		_, ok, err := s.GetCachedCompare(ctx, "org1", "repo1", basehead, now)
		require.NoError(t, err)
		return ok
	}

	put("main...feat", "main", "feat")
	put("feat...main", "feat", "main")
	put("dev...other", "dev", "other")

	c, ok, err := s.GetCachedCompare(ctx, "org1", "repo1", "main...feat", now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 200, c.Status, "the stored status must round-trip")
	assert.Equal(t, "main", c.BaseRef)
	assert.Equal(t, "feat", c.HeadRef)

	require.NoError(t, s.InvalidateCompareForRef(ctx, "org1", "repo1", "main"))
	assert.False(t, serves("main...feat"), "a base-side match must flush")
	assert.False(t, serves("feat...main"), "a head-side match must flush")
	assert.True(t, serves("dev...other"), "a comparison not naming the ref must survive")
}

// TestInvalidateCommitsListForRef: the per-ref flush drops one requested ref
// spelling's snapshots (the empty ref is the default-branch listing's own
// spelling) and leaves other refs' snapshots and the commit rows alone.
func TestInvalidateCommitsListForRef(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	commit := CachedGitCommit{
		Owner: "org1", Repo: "repo1", SHA: testSHA,
		TreeSHA: "1111111111111111111111111111111111111111", Message: "m",
	}
	for _, ref := range []string{"main", "claude/dev", ""} {
		require.NoError(t, s.PutCachedCommitsList(ctx, "org1", "repo1", ref, 30, 1,
			[]CachedGitCommit{commit}, now, time.Hour))
	}
	serves := func(ref string) bool {
		t.Helper()
		_, ok, err := s.GetCachedCommitsList(ctx, "org1", "repo1", ref, 30, 1, now)
		require.NoError(t, err)
		return ok
	}

	require.NoError(t, s.InvalidateCommitsListForRef(ctx, "org1", "repo1", "main"))
	assert.False(t, serves("main"), "the ref's snapshot must be gone")
	assert.True(t, serves("claude/dev"), "another ref's snapshot must survive")
	assert.True(t, serves(""), "the default-branch spelling is its own key and must survive")

	require.NoError(t, s.InvalidateCommitsListForRef(ctx, "org1", "repo1", ""))
	assert.False(t, serves(""), "the empty-ref spelling flushes on its own")

	_, ok, err := s.GetCachedGitCommit(ctx, "org1", "repo1", testSHA, now)
	require.NoError(t, err)
	assert.True(t, ok, "the immutable commit rows must survive snapshot flushes")
}

// TestInvalidateContentsForRef: the per-ref flush drops one requested ref
// spelling's contents rows (the empty ref is the default-branch spelling)
// and leaves other refs' rows alone.
func TestInvalidateContentsForRef(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	for _, ref := range []string{"main", "claude/dev", ""} {
		require.NoError(t, s.PutCachedContents(ctx, CachedContents{
			Owner: "org1", Repo: "repo1", Path: "README.md", Ref: ref,
			Kind: ContentsKindFile, Name: "README.md", Content: "aGk=", Encoding: "base64",
		}, now, time.Hour))
	}
	serves := func(ref string) bool {
		t.Helper()
		_, ok, err := s.GetCachedContents(ctx, "org1", "repo1", "README.md", ref, now)
		require.NoError(t, err)
		return ok
	}

	require.NoError(t, s.InvalidateContentsForRef(ctx, "org1", "repo1", "main"))
	assert.False(t, serves("main"), "the ref's rows must be gone")
	assert.True(t, serves("claude/dev"), "another ref's rows must survive")
	assert.True(t, serves(""), "the default-branch spelling is its own key and must survive")

	require.NoError(t, s.InvalidateContentsForRef(ctx, "org1", "repo1", ""))
	assert.False(t, serves(""), "the empty-ref spelling flushes on its own")
}
