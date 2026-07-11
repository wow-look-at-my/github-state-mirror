package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// This file covers round 2's per-ref invalidation matrix end to end through
// the dispatcher: pushes flush exactly the pushed ref's slice of the
// ref-keyed caches (falling back repo-wide only without a usable ref),
// CI/workflow_job events flush the refs/shas their payloads name, and
// pull_request events clear the PR's pull-diff-406 verdict.

const (
	r2SHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r2OtherSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// r2Seeder seeds and probes the four ref-keyed caches for org1/repo1.
type r2Seeder struct {
	t     *testing.T
	store *ghdata.Store
	now   time.Time
}

func (s r2Seeder) seedContents(ref string) {
	s.t.Helper()
	require.NoError(s.t, s.store.PutCachedContents(context.Background(), ghdata.CachedContents{
		Owner: "org1", Repo: "repo1", Path: "README.md", Ref: ref,
		Kind: ghdata.ContentsKindFile, Name: "README.md", Content: "aGk=", Encoding: "base64",
	}, s.now, time.Hour))
}

func (s r2Seeder) contentsServe(ref string) bool {
	s.t.Helper()
	_, ok, err := s.store.GetCachedContents(context.Background(), "org1", "repo1", "README.md", ref, s.now)
	require.NoError(s.t, err)
	return ok
}

func (s r2Seeder) seedCommitsList(ref string) {
	s.t.Helper()
	require.NoError(s.t, s.store.PutCachedCommitsList(context.Background(), "org1", "repo1", ref, 30, 1,
		[]ghdata.CachedGitCommit{{
			Owner: "org1", Repo: "repo1", SHA: r2SHA,
			TreeSHA: "1111111111111111111111111111111111111111", Message: "m",
		}}, s.now, time.Hour))
}

func (s r2Seeder) commitsListServe(ref string) bool {
	s.t.Helper()
	_, ok, err := s.store.GetCachedCommitsList(context.Background(), "org1", "repo1", ref, 30, 1, s.now)
	require.NoError(s.t, err)
	return ok
}

func (s r2Seeder) seedCommitCI(ref string) {
	s.t.Helper()
	require.NoError(s.t, s.store.PutCachedCommitCI(context.Background(), ghdata.CachedCommitCI{
		Owner: "org1", Repo: "repo1", Ref: ref, Kind: ghdata.CommitCIKindStatus,
		Doc: `{"seeded":true}`,
	}, 30, 1, s.now, time.Hour))
}

func (s r2Seeder) commitCIServe(ref string) bool {
	s.t.Helper()
	_, ok, err := s.store.GetCachedCommitCI(context.Background(), "org1", "repo1", ref, ghdata.CommitCIKindStatus, 30, 1, s.now)
	require.NoError(s.t, err)
	return ok
}

func (s r2Seeder) seedCompare(basehead, baseRef, headRef string) {
	s.t.Helper()
	require.NoError(s.t, s.store.PutCachedCompare(context.Background(), ghdata.CachedCompare{
		Owner: "org1", Repo: "repo1", Basehead: basehead,
		BaseRef: baseRef, HeadRef: headRef, Status: 200, Doc: `{"status":"ahead"}`,
	}, nil, s.now, time.Hour))
}

func (s r2Seeder) compareServe(basehead string) bool {
	s.t.Helper()
	_, ok, err := s.store.GetCachedCompare(context.Background(), "org1", "repo1", basehead, s.now)
	require.NoError(s.t, err)
	return ok
}

func (s r2Seeder) seedWorkflowRuns(sha string) {
	s.t.Helper()
	require.NoError(s.t, s.store.PutCachedWorkflowRuns(context.Background(), "org1", "repo1", sha, 30, 1,
		`{"total_count":1}`, s.now, time.Hour))
}

func (s r2Seeder) workflowRunsServe(sha string) bool {
	s.t.Helper()
	_, ok, err := s.store.GetCachedWorkflowRuns(context.Background(), "org1", "repo1", sha, 30, 1, s.now)
	require.NoError(s.t, err)
	return ok
}

// TestDispatch_PushToBranch_FlushesOnlyThatRef: a push to branch X flushes
// X's contents/commits-list/commit-CI rows and the comparisons naming X on
// either side -- while branch Y's rows, the default-branch (empty-ref) rows,
// and comparisons not touching X all survive.
func TestDispatch_PushToBranch_FlushesOnlyThatRef(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	for _, ref := range []string{"feat", "other", ""} {
		s.seedContents(ref)
		s.seedCommitsList(ref)
	}
	s.seedCommitCI("feat")
	s.seedCommitCI("other")
	s.seedCompare("main...feat", "main", "feat")
	s.seedCompare("feat...other", "feat", "other")
	s.seedCompare("main...other", "main", "other")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", []byte(`{
		"ref": "refs/heads/feat",
		"repository": {"name": "repo1", "full_name": "org1/repo1", "default_branch": "main",
			"owner": {"login": "org1"}}
	}`)))

	assert.False(t, s.contentsServe("feat"), "the pushed branch's contents rows must flush")
	assert.False(t, s.commitsListServe("feat"), "the pushed branch's commits-list snapshots must flush")
	assert.False(t, s.commitCIServe("feat"), "the pushed branch's commit-CI snapshots must flush")
	assert.False(t, s.compareServe("main...feat"), "a comparison with the pushed branch as head must flush")
	assert.False(t, s.compareServe("feat...other"), "a comparison with the pushed branch as base must flush")

	assert.True(t, s.contentsServe("other"), "another branch's contents rows must survive")
	assert.True(t, s.commitsListServe("other"), "another branch's commits-list snapshots must survive")
	assert.True(t, s.commitCIServe("other"), "another branch's commit-CI snapshots must survive")
	assert.True(t, s.contentsServe(""), "a non-default-branch push must leave the default-branch rows")
	assert.True(t, s.commitsListServe(""), "a non-default-branch push must leave the default-branch listing")
	assert.True(t, s.compareServe("main...other"), "a comparison not naming the pushed branch must survive")
}

// TestDispatch_PushToDefaultBranch_FlushesEmptyRefRows: a default-branch push
// owns TWO spellings of the same rows -- the branch name and the empty ref
// (the default-branch-relative key contents/commits-list use) -- so both
// flush, while an unrelated branch's rows survive.
func TestDispatch_PushToDefaultBranch_FlushesEmptyRefRows(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	for _, ref := range []string{"main", "other", ""} {
		s.seedContents(ref)
		s.seedCommitsList(ref)
	}

	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", []byte(`{
		"ref": "refs/heads/main",
		"repository": {"name": "repo1", "full_name": "org1/repo1", "default_branch": "main",
			"owner": {"login": "org1"}}
	}`)))

	assert.False(t, s.contentsServe("main"), "the default branch's named contents rows must flush")
	assert.False(t, s.contentsServe(""), "the default branch's empty-ref contents rows must flush too")
	assert.False(t, s.commitsListServe("main"), "the default branch's named listing must flush")
	assert.False(t, s.commitsListServe(""), "the default branch's empty-ref listing must flush too")
	assert.True(t, s.contentsServe("other"), "another branch's contents rows must survive")
	assert.True(t, s.commitsListServe("other"), "another branch's listing must survive")
}

// TestDispatch_TagPush_FlushesOnlyThatRef: a refs/tags/T push is a ref move
// like any other -- T's rows flush per-ref, NOT repo-wide, so branch rows and
// the default-branch spelling survive.
func TestDispatch_TagPush_FlushesOnlyThatRef(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	for _, ref := range []string{"v1.2.3", "main", ""} {
		s.seedContents(ref)
		s.seedCommitsList(ref)
	}
	s.seedCommitCI("v1.2.3")
	s.seedCommitCI("tags/v1.2.3")
	s.seedCommitCI("refs/tags/v1.2.3")
	s.seedCommitCI("main")
	s.seedCompare("main...v1.2.3", "main", "v1.2.3")
	s.seedCompare("main...other", "main", "other")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", []byte(`{
		"ref": "refs/tags/v1.2.3",
		"repository": {"name": "repo1", "full_name": "org1/repo1", "default_branch": "main",
			"owner": {"login": "org1"}}
	}`)))

	assert.False(t, s.contentsServe("v1.2.3"), "the pushed tag's contents rows must flush")
	assert.False(t, s.commitsListServe("v1.2.3"), "the pushed tag's listing must flush")
	assert.False(t, s.commitCIServe("v1.2.3"), "the pushed tag's commit-CI snapshots must flush")
	assert.False(t, s.commitCIServe("tags/v1.2.3"), "the tag's tags/<name> spelling must flush too")
	assert.False(t, s.commitCIServe("refs/tags/v1.2.3"), "the tag's refs/tags/<name> spelling must flush too")
	assert.False(t, s.compareServe("main...v1.2.3"), "a comparison naming the tag must flush")

	assert.True(t, s.contentsServe("main"), "a tag push must not flush a branch's rows")
	assert.True(t, s.commitsListServe("main"), "a tag push must not flush a branch's listing")
	assert.True(t, s.commitCIServe("main"), "a tag push must not flush a branch's commit-CI snapshots")
	assert.True(t, s.contentsServe(""), "a tag push must leave the default-branch rows")
	assert.True(t, s.commitsListServe(""), "a tag push must leave the default-branch listing")
	assert.True(t, s.compareServe("main...other"), "a comparison not naming the tag must survive")
}

// TestDispatch_UnparseablePush_FallsBackRepoWide: with no usable ref (here an
// unparseable body -- the event still names the repo) every ref-keyed cache
// keeps the old conservative repo-wide flush.
func TestDispatch_UnparseablePush_FallsBackRepoWide(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	for _, ref := range []string{"feat", "other", ""} {
		s.seedContents(ref)
		s.seedCommitsList(ref)
	}
	s.seedCommitCI("feat")
	s.seedCompare("main...feat", "main", "feat")

	dispatcher.Dispatch(ctx, webhook.Event{
		Type:           "push",
		RepoOwnerLogin: "org1",
		RepoNameStr:    "repo1",
	})

	for _, ref := range []string{"feat", "other", ""} {
		assert.False(t, s.contentsServe(ref), "repo-wide fallback must flush contents ref %q", ref)
		assert.False(t, s.commitsListServe(ref), "repo-wide fallback must flush commits-list ref %q", ref)
	}
	assert.False(t, s.commitCIServe("feat"), "repo-wide fallback must flush commit-CI snapshots")
	assert.False(t, s.compareServe("main...feat"), "repo-wide fallback must flush compare docs")
}

// TestDispatch_CheckRun_FlushesNamedRefsAndWorkflowRuns: a check_run event
// flushes the commit-CI snapshots for its head_sha AND head_branch spellings
// plus the sha's workflow-runs pages -- another sha's rows survive both.
func TestDispatch_CheckRun_FlushesNamedRefsAndWorkflowRuns(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	s.seedCommitCI(r2SHA)
	s.seedCommitCI("feat")
	s.seedCommitCI("heads/feat")
	s.seedCommitCI("refs/heads/feat")
	s.seedCommitCI(r2OtherSHA)
	s.seedWorkflowRuns(r2SHA)
	s.seedWorkflowRuns(r2OtherSHA)

	dispatcher.Dispatch(ctx, webhook.ParseEvent("check_run", []byte(`{
		"action": "completed",
		"check_run": {"head_sha": "`+r2SHA+`", "status": "completed", "conclusion": "success",
			"name": "build", "check_suite": {"head_branch": "feat"}},
		"repository": {"name": "repo1", "owner": {"login": "org1"}}
	}`)))

	assert.False(t, s.commitCIServe(r2SHA), "the head sha's commit-CI snapshots must flush")
	assert.False(t, s.commitCIServe("feat"), "the head branch's commit-CI snapshots must flush")
	assert.False(t, s.commitCIServe("heads/feat"), "the branch's heads/<name> spelling must flush too")
	assert.False(t, s.commitCIServe("refs/heads/feat"), "the branch's refs/heads/<name> spelling must flush too")
	assert.True(t, s.commitCIServe(r2OtherSHA), "another sha's commit-CI snapshots must survive")
	assert.False(t, s.workflowRunsServe(r2SHA), "the head sha's workflow-runs pages must flush")
	assert.True(t, s.workflowRunsServe(r2OtherSHA), "another sha's workflow-runs pages must survive")
}

// TestDispatch_WorkflowJob_FlushesWorkflowRunsForSHA: a workflow_job delivery
// flushes the job sha's workflow-runs pages -- for EVERY action, including
// the queued one the disposition logic drops as ignored, because the
// invalidation pass runs before it (a queued job is exactly a run the cached
// listing may not have shown yet).
func TestDispatch_WorkflowJob_FlushesWorkflowRunsForSHA(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	// makeWorkflowJobPayload pins head_sha "cafe1234".
	s.seedWorkflowRuns("cafe1234")
	s.seedWorkflowRuns(r2OtherSHA)

	result := dispatcher.Dispatch(ctx, webhook.ParseEvent("workflow_job",
		makeWorkflowJobPayload(t, "queued", "org1", "repo1", 42, "build", "queued", "")))

	assert.Equal(t, webhook.DispIgnored, result.Disposition, "queued stays ignored for the job table")
	assert.False(t, s.workflowRunsServe("cafe1234"), "the job sha's workflow-runs pages must flush even on queued")
	assert.True(t, s.workflowRunsServe(r2OtherSHA), "another sha's workflow-runs pages must survive")
}

// TestDispatch_PullRequest_ClearsPullDiff406: a pull_request event clears
// that one PR's cached 406 diff verdict (its head just moved, so the diff
// may have shrunk back under the boundary) and leaves another PR's verdict.
func TestDispatch_PullRequest_ClearsPullDiff406(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, store.PutCachedPullDiff406(ctx, "org1", "repo1", 42, `{"message":"too big"}`, now, time.Hour))
	require.NoError(t, store.PutCachedPullDiff406(ctx, "org1", "repo1", 7, `{"message":"too big"}`, now, time.Hour))

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request",
		makePRPayload(t, "synchronize", "open", "org1", "repo1", 42, "t")))

	_, ok, err := store.GetCachedPullDiff406(ctx, "org1", "repo1", 42, now)
	require.NoError(t, err)
	assert.False(t, ok, "the PR's 406 verdict must be cleared")
	_, ok, err = store.GetCachedPullDiff406(ctx, "org1", "repo1", 7, now)
	require.NoError(t, err)
	assert.True(t, ok, "another PR's 406 verdict must survive")
}

// TestDispatch_Push_FlushesAlternateRefSpellings: the cached routes key rows
// by the VERBATIM requested ref, and GitHub also accepts the heads/<name> and
// refs/heads/<name> spellings on the CI routes, contents ?ref=, commits
// ?sha=, and compare baseheads -- so a push to main flushes ALL of main's
// spellings, while an unrelated ref's rows (in any spelling) survive.
func TestDispatch_Push_FlushesAlternateRefSpellings(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	s.seedCommitCI("heads/main")
	s.seedCommitCI("refs/heads/main")
	s.seedCommitCI("heads/other")
	s.seedContents("refs/heads/main")
	s.seedContents("heads/main")
	s.seedContents("refs/heads/other")
	s.seedCommitsList("refs/heads/main")
	s.seedCompare("refs/heads/main...feat", "refs/heads/main", "feat")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", []byte(`{
		"ref": "refs/heads/main",
		"repository": {"name": "repo1", "full_name": "org1/repo1", "default_branch": "main",
			"owner": {"login": "org1"}}
	}`)))

	assert.False(t, s.commitCIServe("heads/main"), "the heads/<name> commit-CI spelling must flush")
	assert.False(t, s.commitCIServe("refs/heads/main"), "the refs/heads/<name> commit-CI spelling must flush")
	assert.True(t, s.commitCIServe("heads/other"), "an unrelated ref's commit-CI rows must survive")
	assert.False(t, s.contentsServe("refs/heads/main"), "the refs/heads/<name> contents spelling must flush")
	assert.False(t, s.contentsServe("heads/main"), "the heads/<name> contents spelling must flush")
	assert.True(t, s.contentsServe("refs/heads/other"), "an unrelated ref's contents rows must survive")
	assert.False(t, s.commitsListServe("refs/heads/main"), "the refs/heads/<name> commits-list spelling must flush")
	assert.False(t, s.compareServe("refs/heads/main...feat"), "a comparison naming a qualified spelling must flush")
}

// TestDispatch_WorkflowRun_FlushesWorkflowRunsForSHA: a workflow_run delivery
// flushes its head_sha's cached workflow-runs pages -- the ONLY invalidation
// signal for a startup_failure run, which creates no jobs, check runs, or
// statuses. The truth side has no workflow_run handler, so the delivery still
// records as ignored: invalidation runs before the disposition logic, the
// queued-workflow_job precedent.
func TestDispatch_WorkflowRun_FlushesWorkflowRunsForSHA(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	s.seedWorkflowRuns(r2SHA)
	s.seedWorkflowRuns(r2OtherSHA)

	result := dispatcher.Dispatch(ctx, webhook.ParseEvent("workflow_run", []byte(`{
		"action": "completed",
		"workflow_run": {"id": 99, "head_sha": "`+r2SHA+`", "status": "completed",
			"conclusion": "startup_failure"},
		"repository": {"name": "repo1", "owner": {"login": "org1"}}
	}`)))

	assert.Equal(t, webhook.DispIgnored, result.Disposition, "no truth-side handler; the delivery stays ignored")
	assert.False(t, s.workflowRunsServe(r2SHA), "the run's head sha's workflow-runs pages must flush")
	assert.True(t, s.workflowRunsServe(r2OtherSHA), "another sha's workflow-runs pages must survive")
}

// TestFlushWorkflowRunsForSHA_EmptySHA_FallsBackRepoWide: the shared
// workflow-runs flush widens to the whole repo when the caller has no sha --
// an empty sha would exact-match nothing, silently flushing NOTHING while the
// triggering payload still said some run changed. (ParseCheckPayload requires
// a sha today, so the CI-event path cannot reach this; the fallback is what
// keeps a future parser relaxation from turning that flush into a no-op.)
func TestFlushWorkflowRunsForSHA_EmptySHA_FallsBackRepoWide(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	s.seedWorkflowRuns(r2SHA)
	s.seedWorkflowRuns(r2OtherSHA)

	dispatcher.flushWorkflowRunsForSHA(ctx, "org1/repo1", "org1", "repo1", "")

	assert.False(t, s.workflowRunsServe(r2SHA), "an empty sha must flush repo-wide, not no-op")
	assert.False(t, s.workflowRunsServe(r2OtherSHA), "an empty sha must flush repo-wide, not no-op")
}
