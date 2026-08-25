package sync

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The dispatcher restates the API package's synthetic commits_list_cache ref
func TestPullCommitsRefKey(t *testing.T) {
	require.Equal(t, "pull/7/commits", pullCommitsRefKey(7))
}
