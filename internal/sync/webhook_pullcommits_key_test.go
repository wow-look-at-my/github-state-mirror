package sync

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The dispatcher restates the API package's synthetic commits_list_cache ref
// key for a PR's commit list (a sync -> api import would be a cycle). If the
// two ever drift, the per-PR flush silently matches nothing and the snapshot
// serves stale until the repo-wide push flush or the TTL catches it -- so both
// sides pin the same literal, and the api package's end-to-end flush test
// proves they still meet.
func TestPullCommitsRefKey(t *testing.T) {
	require.Equal(t, "pull/7/commits", pullCommitsRefKey(7))
}
