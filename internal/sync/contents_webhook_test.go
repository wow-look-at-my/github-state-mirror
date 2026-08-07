package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

func TestDispatch_PushInvalidatesContentsByChangedPath(t *testing.T) {
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
	ctx := context.Background()

	changedKey := RepoContentsKey("my-org", "my-repo", "docs/README.md", "ref=main")
	unchangedKey := RepoContentsKey("my-org", "my-repo", "docs/unchanged.md", "ref=main")
	seed(t, mgr, KindRepoContents, changedKey)
	seed(t, mgr, KindRepoContents, unchangedKey)

	raw := []byte(`{
		"repository": {"name": "my-repo", "owner": {"login": "my-org"}},
		"commits": [{"modified": ["docs/README.md"]}]
	}`)
	result := dispatcher.Dispatch(ctx, webhook.ParseEvent("push", raw))
	assert.Equal(t, webhook.DispInvalidated, result.Disposition)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindRepoContents, Key: changedKey})
	require.NoError(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)

	meta, err = fStore.Get(ctx, freshness.ResourceID{Kind: KindRepoContents, Key: unchangedKey})
	require.NoError(t, err)
	assert.Equal(t, freshness.StateFresh, meta.State)
}

func TestDispatch_PushInvalidatesAllRepoContentsWithoutChangedPathList(t *testing.T) {
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
	ctx := context.Background()

	keyA := RepoContentsKey("my-org", "my-repo", "docs/README.md", "ref=main")
	keyB := RepoContentsKey("my-org", "my-repo", "src/main.go", "ref=main")
	otherRepoKey := RepoContentsKey("my-org", "other-repo", "docs/README.md", "ref=main")
	seed(t, mgr, KindRepoContents, keyA)
	seed(t, mgr, KindRepoContents, keyB)
	seed(t, mgr, KindRepoContents, otherRepoKey)

	raw := []byte(`{
		"repository": {"name": "my-repo", "owner": {"login": "my-org"}}
	}`)
	result := dispatcher.Dispatch(ctx, webhook.ParseEvent("push", raw))
	assert.Equal(t, webhook.DispInvalidated, result.Disposition)

	for _, key := range []string{keyA, keyB} {
		meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindRepoContents, Key: key})
		require.NoError(t, err)
		assert.Equal(t, freshness.StateStale, meta.State)
	}
	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindRepoContents, Key: otherRepoKey})
	require.NoError(t, err)
	assert.Equal(t, freshness.StateFresh, meta.State)
}
