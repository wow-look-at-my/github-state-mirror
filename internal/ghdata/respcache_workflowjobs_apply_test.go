package ghdata

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const applyRunID = int64(9001)

func jobEntry(id int64, attempt int64, status string) StoredWorkflowJob {
	return StoredWorkflowJob{
		ID: id, RunID: applyRunID, RunAttempt: attempt, HeadSHA: ciSHA,
		Name: "build", Status: status, Labels: []string{"wow-linux"},
	}
}

func seedJobsPage(t *testing.T, s *Store, jobs ...StoredWorkflowJob) {
	t.Helper()
	doc, err := MarshalCacheDoc(StoredRunJobsPage{TotalCount: int64(len(jobs)), Jobs: jobs})
	require.NoError(t, err)
	require.NoError(t, s.PutCachedWorkflowJobs(context.Background(), "org1", "repo1",
		WorkflowJobsKindRunJobs, applyRunID, applyRunID, 30, 1, string(doc), time.Now(), time.Hour))
}

func readJobsPage(t *testing.T, s *Store) (StoredRunJobsPage, bool) {
	t.Helper()
	doc, ok, err := s.GetCachedWorkflowJobs(context.Background(), "org1", "repo1",
		WorkflowJobsKindRunJobs, applyRunID, 30, 1, time.Now())
	require.NoError(t, err)
	if !ok {
		return StoredRunJobsPage{}, false
	}
	var page StoredRunJobsPage
	require.NoError(t, json.Unmarshal([]byte(doc), &page))
	return page, true
}

// The delivery states the job whole, so its entry is replaced and every other
// job on the page is left exactly as it was.
func TestApplyWorkflowJob_RewritesOneEntry(t *testing.T) {
	s := testStore(t)
	seedJobsPage(t, s, jobEntry(1, 1, "queued"), jobEntry(2, 1, "in_progress"))

	done := jobEntry(2, 1, "completed")
	concl := "success"
	done.Conclusion = &concl
	applied, err := s.ApplyWorkflowJob(context.Background(), "org1", "repo1", applyRunID, done,
		time.Now(), WorkflowJobsLiveTTL, WorkflowJobsCacheTTL)
	require.NoError(t, err)
	require.True(t, applied)

	page, ok := readJobsPage(t, s)
	require.True(t, ok)
	assert.Equal(t, "queued", page.Jobs[0].Status, "the other job is untouched")
	assert.Equal(t, "completed", page.Jobs[1].Status)
	require.NotNil(t, page.Jobs[1].Conclusion)
	assert.Equal(t, "success", *page.Jobs[1].Conclusion)
}

// A page still holding a moving job keeps the LIVE clock; once the last one
// settles the same page earns the long one. The TTL follows the answer, not
// the delivery.
func TestApplyWorkflowJob_TTLFollowsWhatIsLeftMoving(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedJobsPage(t, s, jobEntry(1, 1, "queued"), jobEntry(2, 1, "in_progress"))

	expiryOf := func() time.Time {
		t.Helper()
		var raw string
		require.NoError(t, s.db.QueryRow(`SELECT expires_at FROM workflow_jobs_cache`).Scan(&raw))
		at, err := time.Parse(time.RFC3339, raw)
		require.NoError(t, err)
		return at
	}

	now := time.Now()
	_, err := s.ApplyWorkflowJob(ctx, "org1", "repo1", applyRunID, jobEntry(2, 1, "completed"),
		now, WorkflowJobsLiveTTL, WorkflowJobsCacheTTL)
	require.NoError(t, err)
	assert.WithinDuration(t, now.Add(WorkflowJobsLiveTTL), expiryOf(), 2*time.Second,
		"job 1 is still queued, so the page is still live")

	_, err = s.ApplyWorkflowJob(ctx, "org1", "repo1", applyRunID, jobEntry(1, 1, "completed"),
		now, WorkflowJobsLiveTTL, WorkflowJobsCacheTTL)
	require.NoError(t, err)
	assert.WithinDuration(t, now.Add(WorkflowJobsCacheTTL), expiryOf(), 2*time.Second,
		"nothing left moving: the page has settled")
}

// The two things a delivery cannot answer, both of which drop the run's rows
// so a fetch can settle them.
func TestApplyWorkflowJob_FlushesWhatItCannotAnswer(t *testing.T) {
	ctx := context.Background()

	t.Run("a job the page does not list", func(t *testing.T) {
		s := testStore(t)
		seedJobsPage(t, s, jobEntry(1, 1, "completed"))
		applied, err := s.ApplyWorkflowJob(ctx, "org1", "repo1", applyRunID, jobEntry(77, 1, "queued"),
			time.Now(), WorkflowJobsLiveTTL, WorkflowJobsCacheTTL)
		require.NoError(t, err)
		assert.False(t, applied, "the run gained a job; only a fetch settles membership")
		_, ok := readJobsPage(t, s)
		assert.False(t, ok, "the stale page is dropped rather than left short a job")
	})

	t.Run("a different run attempt", func(t *testing.T) {
		s := testStore(t)
		seedJobsPage(t, s, jobEntry(1, 1, "completed"))
		applied, err := s.ApplyWorkflowJob(ctx, "org1", "repo1", applyRunID, jobEntry(1, 2, "queued"),
			time.Now(), WorkflowJobsLiveTTL, WorkflowJobsCacheTTL)
		require.NoError(t, err)
		assert.False(t, applied, "a re-run is a different set of jobs, not an edit to this one")
		_, ok := readJobsPage(t, s)
		assert.False(t, ok)
	})
}

// A run has many single-job rows and a delivery names one. The others are not
// about this delivery at all, so skipping them must not read as "cannot
// absorb" and drag the whole run into a flush.
func TestApplyWorkflowJob_LeavesOtherJobsRowsAlone(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedJobsPage(t, s, jobEntry(1, 1, "in_progress"), jobEntry(2, 1, "in_progress"))
	for _, id := range []int64{1, 2} {
		doc, err := MarshalCacheDoc(jobEntry(id, 1, "in_progress"))
		require.NoError(t, err)
		require.NoError(t, s.PutCachedWorkflowJobs(ctx, "org1", "repo1", WorkflowJobsKindJob,
			id, applyRunID, 1, 1, string(doc), time.Now(), time.Hour))
	}

	applied, err := s.ApplyWorkflowJob(ctx, "org1", "repo1", applyRunID, jobEntry(1, 1, "completed"),
		time.Now(), WorkflowJobsLiveTTL, WorkflowJobsCacheTTL)
	require.NoError(t, err)
	require.True(t, applied)

	for id, want := range map[int64]string{1: "completed", 2: "in_progress"} {
		doc, ok, err := s.GetCachedWorkflowJobs(ctx, "org1", "repo1", WorkflowJobsKindJob, id, 1, 1, time.Now())
		require.NoError(t, err)
		require.True(t, ok, "job %d's row must survive", id)
		var stored StoredWorkflowJob
		require.NoError(t, json.Unmarshal([]byte(doc), &stored))
		assert.Equal(t, want, stored.Status, "job %d", id)
	}
}
