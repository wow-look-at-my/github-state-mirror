package ghdata

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refDoc renders a stored ref answer the way the api layer does.
func refDoc(t *testing.T, ref, sha string) string {
	t.Helper()
	var doc storedGitRefDoc
	doc.Ref, doc.NodeID = ref, "REF_kwAE"
	doc.Object.SHA, doc.Object.Type = sha, "commit"
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return string(b)
}

func putRef(t *testing.T, s *Store, ref, sha string, status int) {
	t.Helper()
	require.NoError(t, s.PutCachedGitRef(context.Background(), CachedGitRef{
		Owner: "org1", Repo: "repo1", Ref: ref, Status: status,
		Doc: refDoc(t, "refs/heads/main", sha),
	}, time.Now(), GitRefCacheTTL))
}

const tipSHA = "cccccccccccccccccccccccccccccccccccccccc"

func TestKnownBranchTip(t *testing.T) {
	ctx, now := context.Background(), time.Now()

	t.Run("unknown branch reports no information", func(t *testing.T) {
		s := testStore(t)
		_, known, err := s.KnownBranchTip(ctx, "org1", "repo1", "main", now)
		require.NoError(t, err)
		assert.False(t, known, "never-asked-for is not a mismatch, and callers must be able to tell")
	})

	// Rows key the verbatim requested spelling, so the tip is wherever the
	// caller who asked for it put it -- any spelling answers for the branch.
	for _, stored := range []string{"main", "heads/main", "refs/heads/main"} {
		for _, asked := range []string{"main", "heads/main", "refs/heads/main"} {
			t.Run("stored "+stored+" asked "+asked, func(t *testing.T) {
				s := testStore(t)
				putRef(t, s, stored, tipSHA, http.StatusOK)
				got, known, err := s.KnownBranchTip(ctx, "org1", "repo1", asked, now)
				require.NoError(t, err)
				require.True(t, known)
				assert.Equal(t, tipSHA, got)
			})
		}
	}

	// An absent-ref verdict says the branch does not exist. It is not a tip,
	// and reading its doc as one would compare a comparison against nothing.
	t.Run("404 verdict is not a tip", func(t *testing.T) {
		s := testStore(t)
		require.NoError(t, s.PutCachedGitRef(ctx, CachedGitRef{
			Owner: "org1", Repo: "repo1", Ref: "heads/main", Status: http.StatusNotFound,
			Doc: `{"message":"Not Found","status":"404"}`,
		}, now, GitRefCacheTTL))
		_, known, err := s.KnownBranchTip(ctx, "org1", "repo1", "main", now)
		require.NoError(t, err)
		assert.False(t, known)
	})

	// An annotated tag's ref object is the TAG object, not the commit it
	// points at, so its sha would never equal a comparison's base tip and
	// every tag-based comparison would read as moved forever.
	t.Run("tags are out of scope", func(t *testing.T) {
		s := testStore(t)
		putRef(t, s, "tags/v1", tipSHA, http.StatusOK)
		for _, spelling := range []string{"tags/v1", "refs/tags/v1"} {
			_, known, err := s.KnownBranchTip(ctx, "org1", "repo1", spelling, now)
			require.NoError(t, err)
			assert.False(t, known, spelling)
		}
	})

	t.Run("expired rows report no information", func(t *testing.T) {
		s := testStore(t)
		putRef(t, s, "heads/main", tipSHA, http.StatusOK)
		_, known, err := s.KnownBranchTip(ctx, "org1", "repo1", "main", now.Add(GitRefCacheTTL+time.Minute))
		require.NoError(t, err)
		assert.False(t, known)
	})

	// A row whose document cannot be read, or whose object sha is not a real
	// oid, is not evidence of anything -- least of all of a move.
	t.Run("unreadable rows report no information", func(t *testing.T) {
		for name, doc := range map[string]string{
			"not json":  `{`,
			"empty sha": `{"ref":"refs/heads/main","node_id":"n","object":{"sha":"","type":"commit"}}`,
			"short sha": `{"ref":"refs/heads/main","node_id":"n","object":{"sha":"abc","type":"commit"}}`,
		} {
			t.Run(name, func(t *testing.T) {
				s := testStore(t)
				require.NoError(t, s.PutCachedGitRef(ctx, CachedGitRef{
					Owner: "org1", Repo: "repo1", Ref: "heads/main", Status: http.StatusOK, Doc: doc,
				}, now, GitRefCacheTTL))
				_, known, err := s.KnownBranchTip(ctx, "org1", "repo1", "main", now)
				require.NoError(t, err)
				assert.False(t, known)
			})
		}
	})
}
