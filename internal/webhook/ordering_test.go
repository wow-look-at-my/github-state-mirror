package webhook

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func orderEvent(t *testing.T, eventType string, payload map[string]any) (EventOrder, bool) {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return OrderOf(Event{Type: eventType, Raw: raw})
}

// repoObj mirrors the real shape, pushed_at included, so a decode failure cannot hide behind an absent field.
func repoObj(extra map[string]any) map[string]any {
	m := map[string]any{
		"name": "repo1", "full_name": "Org1/Repo1", "owner": map[string]any{"login": "Org1"},
		"pushed_at": "2026-08-14T14:00:00Z",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// A push's clock is GitHub's record of the push, never the commit date the
// committer's machine wrote. head_commit.timestamp survives a rebase unchanged
// and can be arbitrarily far in the future; ordering on it would let a crafted
// commit date pin a branch's watermark ahead of every real push.
func TestOrderOf_PushUsesPushedAtNotTheCommitDate(t *testing.T) {
	pushedAt := time.Date(2026, 8, 14, 15, 35, 26, 0, time.UTC)
	order, ok := orderEvent(t, "push", map[string]any{
		"ref":         "refs/heads/master",
		"head_commit": map[string]any{"id": "abc", "timestamp": "2031-01-01T00:00:00Z"},
		"repository":  repoObj(map[string]any{"pushed_at": pushedAt.Unix()}),
	})
	require.True(t, ok)
	assert.Equal(t, "ref:org1/repo1:refs/heads/master", order.Subject)
	assert.Equal(t, pushedAt, order.At)
	assert.Equal(t, "repository.pushed_at", order.Field)
}

// Push payloads send pushed_at as a Unix integer; every other event sends
// RFC3339. Both have to read.
func TestOrderOf_PushedAtAcceptsBothShapes(t *testing.T) {
	order, ok := orderEvent(t, "push", map[string]any{
		"ref": "refs/heads/main", "repository": repoObj(map[string]any{"pushed_at": "2026-08-14T15:35:26Z"}),
	})
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 8, 14, 15, 35, 26, 0, time.UTC), order.At)
}

// A push with no pushed_at at all is unorderable rather than assigned a
// fabricated time: it must still apply.
func TestOrderOf_PushWithoutPushedAtIsUnorderable(t *testing.T) {
	_, ok := orderEvent(t, "push", map[string]any{
		"ref": "refs/heads/main", "head_commit": map[string]any{"timestamp": "2026-08-14T15:00:00Z"},
		"repository": map[string]any{"name": "repo1", "full_name": "Org1/Repo1", "owner": map[string]any{"login": "Org1"}},
	})
	assert.False(t, ok, "a commit date is not a substitute for the push time")
}

func TestOrderOf_SubjectGrain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		eventType string
		payload   map[string]any
		subject   string
		field     string
	}{
		{
			"pull_request keys the PR", "pull_request",
			map[string]any{"repository": repoObj(nil), "pull_request": map[string]any{"number": 12, "updated_at": "2026-08-14T15:00:00Z"}},
			"pr:org1/repo1#12", "pull_request.updated_at",
		},
		{
			// One commit carries many contexts; they supersede only themselves.
			"status keys sha AND context", "status",
			map[string]any{"repository": repoObj(nil), "sha": "abc123", "context": "ci/build", "updated_at": "2026-08-14T15:00:00Z"},
			"status:org1/repo1:abc123:ci/build", "updated_at",
		},
		{
			"check_run keys the run id", "check_run",
			map[string]any{"repository": repoObj(nil), "check_run": map[string]any{"id": 99, "started_at": "2026-08-14T15:00:00Z"}},
			"check_run:org1/repo1:99", "check_run.started_at",
		},
		{
			"a completed check run prefers completed_at", "check_run",
			map[string]any{"repository": repoObj(nil), "check_run": map[string]any{
				"id": 99, "started_at": "2026-08-14T15:00:00Z", "completed_at": "2026-08-14T15:04:00Z"}},
			"check_run:org1/repo1:99", "check_run.completed_at",
		},
		{
			// A review is its own subject: two reviews on one PR are independent.
			"pull_request_review keys the review", "pull_request_review",
			map[string]any{"repository": repoObj(nil), "review": map[string]any{"id": 7, "submitted_at": "2026-08-14T15:00:00Z"}},
			"review:org1/repo1:7", "review.submitted_at",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order, ok := orderEvent(t, tc.eventType, tc.payload)
			require.True(t, ok)
			assert.Equal(t, tc.subject, order.Subject)
			assert.Equal(t, tc.field, order.Field)
		})
	}
}

// Events the mirror stores nothing orderable for, and payloads missing their
// own clock, report unorderable -- which applies them, rather than refusing
// state over a timestamp that was never there.
func TestOrderOf_UnorderableEvents(t *testing.T) {
	for _, tc := range []struct {
		name      string
		eventType string
		payload   map[string]any
	}{
		{"label carries no timestamp", "label", map[string]any{"repository": repoObj(nil), "label": map[string]any{"name": "bug"}}},
		{"membership stores nothing", "membership", map[string]any{"organization": map[string]any{"login": "org1"}}},
		{"a PR payload with no updated_at", "pull_request", map[string]any{"repository": repoObj(nil), "pull_request": map[string]any{"number": 3}}},
		{"a status with no context", "status", map[string]any{"repository": repoObj(nil), "sha": "abc", "updated_at": "2026-08-14T15:00:00Z"}},
		{"an unparseable body", "push", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.payload == nil {
				_, ok := OrderOf(Event{Type: tc.eventType, Raw: json.RawMessage("not json")})
				assert.False(t, ok)
				return
			}
			_, ok := orderEvent(t, tc.eventType, tc.payload)
			assert.False(t, ok)
		})
	}
}

// Subjects are case-normalized on the repo, so two spellings of one repo share
// a watermark rather than ordering against each other.
func TestOrderOf_RepoKeyIsNormalized(t *testing.T) {
	a, ok := orderEvent(t, "pull_request", map[string]any{
		"repository":   repoObj(map[string]any{"full_name": "Org1/Repo1"}),
		"pull_request": map[string]any{"number": 4, "updated_at": "2026-08-14T15:00:00Z"},
	})
	require.True(t, ok)
	b, ok := orderEvent(t, "pull_request", map[string]any{
		"repository":   repoObj(map[string]any{"full_name": "org1/repo1"}),
		"pull_request": map[string]any{"number": 4, "updated_at": "2026-08-14T15:00:00Z"},
	})
	require.True(t, ok)
	assert.Equal(t, a.Subject, b.Subject)
}
