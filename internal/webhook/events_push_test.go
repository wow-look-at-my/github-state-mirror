package webhook

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBefore = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testMid    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testAfter  = "cccccccccccccccccccccccccccccccccccccccc"
	zeroSHA    = "0000000000000000000000000000000000000000"
)

func pushRaw(t *testing.T, before, after string, forced bool, ids ...string) json.RawMessage {
	t.Helper()
	commits := make([]map[string]interface{}, 0, len(ids))
	for i, id := range ids {
		commits = append(commits, map[string]interface{}{
			"id": id, "tree_id": fmt.Sprintf("%040d", i), "message": fmt.Sprintf("c%d", i),
			"timestamp": "2026-07-03T10:00:00Z",
			"author":    map[string]string{"name": "A", "email": "a@x"},
			"committer": map[string]string{"name": "B", "email": "b@x"},
		})
	}
	raw, err := json.Marshal(map[string]interface{}{
		"repository": map[string]interface{}{"name": "repo1", "owner": map[string]string{"login": "org1"}},
		"before":     before, "after": after, "forced": forced,
		"head_commit": map[string]string{"timestamp": "2026-07-03T10:00:00Z"},
		"commits":     commits,
	})
	require.NoError(t, err)
	return raw
}

func TestParsePushPayload_Commits(t *testing.T) {
	p, err := ParsePushPayload(pushRaw(t, testBefore, testAfter, false, testMid, testAfter))
	require.NoError(t, err)
	assert.Equal(t, "org1", p.Owner)
	assert.Equal(t, testBefore, p.Before)
	assert.Equal(t, testAfter, p.After)
	assert.False(t, p.Forced)
	require.Len(t, p.Commits, 2)
	assert.Equal(t, testMid, p.Commits[0].ID)
	assert.Equal(t, fmt.Sprintf("%040d", 0), p.Commits[0].TreeID)
	assert.Equal(t, "c0", p.Commits[0].Message)
	assert.Equal(t, "2026-07-03T10:00:00Z", p.Commits[0].Timestamp)
	assert.Equal(t, "A", p.Commits[0].AuthorName)
	assert.Equal(t, "b@x", p.Commits[0].CommitterEmail)
}

// TestChainedCommits_LinearChain: the happy path derives commits[0].parent =
// before, commits[i].parent = commits[i-1].
func TestChainedCommits_LinearChain(t *testing.T) {
	p, err := ParsePushPayload(pushRaw(t, testBefore, testAfter, false, testMid, testAfter))
	require.NoError(t, err)
	chain := p.ChainedCommits()
	require.Len(t, chain, 2)
	assert.Equal(t, testBefore, p.ParentForChained(chain, 0))
	assert.Equal(t, testMid, p.ParentForChained(chain, 1))
}

// TestChainedCommits_Declines: every signal that makes the linear derivation
// untrustworthy must yield no chain — wrong parents are worse than a fetch.
func TestChainedCommits_Declines(t *testing.T) {
	t.Run("forced push", func(t *testing.T) {
		p, err := ParsePushPayload(pushRaw(t, testBefore, testAfter, true, testAfter))
		require.NoError(t, err)
		assert.Nil(t, p.ChainedCommits(), "forced: before is not the parent")
	})
	t.Run("new ref (zero before)", func(t *testing.T) {
		p, err := ParsePushPayload(pushRaw(t, zeroSHA, testAfter, false, testAfter))
		require.NoError(t, err)
		assert.Nil(t, p.ChainedCommits(), "a created ref has no before-parent")
	})
	t.Run("last commit is not after (non-linear ordering)", func(t *testing.T) {
		p, err := ParsePushPayload(pushRaw(t, testBefore, testAfter, false, testAfter, testMid))
		require.NoError(t, err)
		assert.Nil(t, p.ChainedCommits(), "array not ending at the tip cannot be a trusted chain")
	})
	t.Run("possibly truncated array", func(t *testing.T) {
		ids := make([]string, maxChainedPushCommits)
		for i := range ids {
			ids[i] = fmt.Sprintf("%039dd", i)[:40]
		}
		ids[len(ids)-1] = testAfter
		p, err := ParsePushPayload(pushRaw(t, testBefore, testAfter, false, ids...))
		require.NoError(t, err)
		assert.Nil(t, p.ChainedCommits(), "at the payload cap the array may be truncated")
	})
	t.Run("empty commits", func(t *testing.T) {
		p, err := ParsePushPayload(pushRaw(t, testBefore, testAfter, false))
		require.NoError(t, err)
		assert.Nil(t, p.ChainedCommits())
	})
}
