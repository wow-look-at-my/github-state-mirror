package api

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// repo-nightmare asks every repo for its open PRs with GitHub's own default
// order spelled out. Before this, the parser rejected both params, so every
// per-repo call forwarded on every graph refresh.
func TestPullsListShapeAcceptsSortAndDirection(t *testing.T) {
	q, err := url.ParseQuery("state=open&sort=created&direction=desc&per_page=20")
	require.NoError(t, err)

	shape, ok := parsePullsListShape(q)
	require.True(t, ok, "the cache holds the complete open set, so it can answer this order")
	assert.Equal(t, "created", shape.sortBy)
	assert.False(t, shape.ascending)
	assert.Equal(t, 20, shape.perPage)
}

func TestPullsListShapeDefaultsToCreatedDescending(t *testing.T) {
	shape, ok := parsePullsListShape(url.Values{})
	require.True(t, ok)
	assert.Equal(t, "created", shape.sortBy, "GitHub's default sort")
	assert.False(t, shape.ascending, "GitHub's default direction")
}

// popularity ranks by comment count and long-running by age against an
// open-state clock. The row carries neither, so a guess would be wrong.
func TestPullsListShapeRefusesSortsTheRowCannotAnswer(t *testing.T) {
	for _, sort := range []string{"popularity", "long-running", "title"} {
		t.Run(sort, func(t *testing.T) {
			_, ok := parsePullsListShape(url.Values{"sort": {sort}})
			assert.False(t, ok, "must keep forwarding rather than invent an order")
		})
	}
	_, ok := parsePullsListShape(url.Values{"direction": {"sideways"}})
	assert.False(t, ok)
}

func pullRow(number int64, created, updated string) dbgen.PullRequest {
	return dbgen.PullRequest{Number: number, CreatedAt: created, UpdatedAt: updated}
}

func TestSortPullRowsOrdersByTheRequestedKey(t *testing.T) {
	rows := []dbgen.PullRequest{
		pullRow(1, "2026-01-01T00:00:00Z", "2026-03-01T00:00:00Z"),
		pullRow(2, "2026-02-01T00:00:00Z", "2026-01-01T00:00:00Z"),
		pullRow(3, "2026-03-01T00:00:00Z", "2026-02-01T00:00:00Z"),
	}

	numbers := func(in []dbgen.PullRequest) []int64 {
		out := make([]int64, 0, len(in))
		for _, pr := range in {
			out = append(out, pr.Number)
		}
		return out
	}

	createdDesc := sortPullRows(rows, pullsListShape{sortBy: "created"})
	assert.Equal(t, []int64{3, 2, 1}, numbers(createdDesc))

	createdAsc := sortPullRows(rows, pullsListShape{sortBy: "created", ascending: true})
	assert.Equal(t, []int64{1, 2, 3}, numbers(createdAsc))

	updatedDesc := sortPullRows(rows, pullsListShape{sortBy: "updated"})
	assert.Equal(t, []int64{1, 3, 2}, numbers(updatedDesc))

	// The input is the caller's slice, and it is served from the shared cache read.
	assert.Equal(t, []int64{1, 2, 3}, numbers(rows), "sorting must not reorder the caller's slice")
}

// PRs created in the same second must not come back in an order that depends
// on the map or scan order behind them.
func TestSortPullRowsBreaksATieByNumber(t *testing.T) {
	same := "2026-01-01T00:00:00Z"
	rows := []dbgen.PullRequest{
		pullRow(7, same, same),
		pullRow(9, same, same),
		pullRow(8, same, same),
	}

	desc := sortPullRows(rows, pullsListShape{sortBy: "created"})
	assert.Equal(t, int64(9), desc[0].Number)
	assert.Equal(t, int64(7), desc[2].Number)

	asc := sortPullRows(rows, pullsListShape{sortBy: "created", ascending: true})
	assert.Equal(t, int64(7), asc[0].Number)
	assert.Equal(t, int64(9), asc[2].Number)
}
