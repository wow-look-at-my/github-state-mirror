package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The list-tier stale-sha gate: the single-PR route refuses to serve a row
// whose merge_commit_sha is the push-invalidated one (PRMergeShaStale, belt
// and braces behind the guarded writes); the list rebuild must apply the
// same belt -- a provably-stale sha renders as null in list items instead of
// being served.
func TestPullsList_GatesPushInvalidatedMergeSha(t *testing.T) {
	router, store, db, _ := respCacheStack(t)
	ctx := t.Context()
	now := time.Now()

	const staleSha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pr := dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 7,
		Title: "t", Url: "u", State: "OPEN",
		CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-02T10:00:00Z",
		NodeID:         sql.NullString{String: "PR_node", Valid: true},
		AuthorLogin:    sql.NullString{String: "alice", Valid: true},
		AuthorType:     sql.NullString{String: "User", Valid: true},
		HeadRefName:    sql.NullString{String: "feature", Valid: true},
		BaseRefName:    sql.NullString{String: "main", Valid: true},
		HeadRefOid:     sql.NullString{String: "1111111111111111111111111111111111111111", Valid: true},
		BaseRefOid:     sql.NullString{String: "2222222222222222222222222222222222222222", Valid: true},
		MergeCommitSha: sql.NullString{String: staleSha, Valid: true},
		Mergeable:      sql.NullString{String: "MERGEABLE", Valid: true},
	}
	require.NoError(t, store.UpsertPR(ctx, pr, now))
	// The list-complete marker, so the route serves from rows.
	require.NoError(t, store.AbsorbPullsList(ctx, "org1", "repo1",
		[]dbgen.PullRequest{pr}, nil, true, now, now, time.Hour))
	// The belt case the guarded writes never produce: force the row's OWN
	// sha to be the live-marked stale one (direct SQL, like a row written
	// before the guard existed or by a raced writer).
	_, err := db.ExecContext(ctx, `UPDATE pull_requests
		SET merge_stale_sha = ?, merge_stale_at = ?
		WHERE owner = 'org1' AND repo = 'repo1' AND number = 7`,
		staleSha, now.UTC().Format(time.RFC3339))
	require.NoError(t, err)

	// The reveal layer: the caller needs a grant for the repo's cached state.
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1",
		Visibility: ghdata.VisibilityPublic, Url: "https://github.com/org1/repo1",
	}))

	req := httptest.NewRequest("GET", "/repos/org1/repo1/pulls", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "hit", w.Header().Get(cacheHeader))

	var items []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Nil(t, items[0]["merge_commit_sha"],
		"a push-invalidated test-merge sha must be gated to null in list items, never served")
}
