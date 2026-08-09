package api

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Single-label route tests. The default body is GitHub's documented `label`
// schema with its url field present, so the rebuild has something to drop.

func defaultLabelUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, `{
		"id": 208045946, "node_id": "MDU6TGFiZWwyMDgwNDU5NDY=",
		"url": "https://api.github.com/repos/org1/repo1/labels/%s",
		"name": %q, "color": "f29513", "default": true,
		"description": "Something isn't working"
	}`, name, name)
}

const labelTarget = "/repos/org1/repo1/labels/auto-pr-merge"

func labelEvent(action, name string) string {
	return fmt.Sprintf(`{"action":%q,"label":{"name":%q,"color":"f29513"},
		"repository":{"name":"repo1","owner":{"login":"org1"}}}`, action, name)
}

func TestCachedLabel_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", labelTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.labelHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.JSONEq(t, `{
		"id": 208045946, "node_id": "MDU6TGFiZWwyMDgwNDU5NDY=",
		"name": "auto-pr-merge", "color": "f29513", "default": true,
		"description": "Something isn't working"
	}`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", labelTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.labelHits), "a hit must not call upstream")
}

// A null description keeps its key: a consumer branches on null vs a string,
// and dropping the field would make the two indistinguishable.
func TestCachedLabel_NullDescriptionKeepsItsKey(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.label = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"id":1,"node_id":"L_1","url":"https://api.github.com/l",
			"name":"auto-pr-merge","color":"ededed","default":false,"description":null}`))
	}

	w := do(t, router, authedReq("GET", labelTarget, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"id":1,"node_id":"L_1","name":"auto-pr-merge",
		"color":"ededed","default":false,"description":null}`, w.Body.String())
	assert.Contains(t, w.Body.String(), `"description":null`)
}

// Every `label` delivery flushes the repo's labels, whatever the action: a
// rename moves two names in one delivery, and `created` is what makes a name
// that did not resolve a moment ago start resolving.
func TestCachedLabel_EveryLabelEventFlushes(t *testing.T) {
	for _, action := range []string{"created", "edited", "deleted"} {
		t.Run(action, func(t *testing.T) {
			router, _, _, u := respCacheStack(t)

			require.Equal(t, "miss", do(t, router, authedReq("GET", labelTarget, nil)).Header().Get(cacheHeader))
			require.Equal(t, "hit", do(t, router, authedReq("GET", labelTarget, nil)).Header().Get(cacheHeader))

			// The delivery names a DIFFERENT label on purpose: the flush grain
			// is the repo, so the cached row must go regardless.
			postWebhook(t, router, "label", labelEvent(action, "some-other-label"))

			w := do(t, router, authedReq("GET", labelTarget, nil))
			assert.Equal(t, "miss", w.Header().Get(cacheHeader), "a label delivery must flush the repo's labels")
			assert.Equal(t, int32(2), atomic.LoadInt32(&u.labelHits))
		})
	}
}

// A repository event (rename/delete/visibility) flushes repo-wide like every
// other cached route.
func TestCachedLabel_RepositoryEventFlushes(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	require.Equal(t, "miss", do(t, router, authedReq("GET", labelTarget, nil)).Header().Get(cacheHeader))
	postWebhook(t, router, "repository", `{"action":"privatized","repository":{"name":"repo1","owner":{"login":"org1"},"private":true,"visibility":"private","default_branch":"main","full_name":"org1/repo1"}}`)

	assert.Equal(t, "miss", do(t, router, authedReq("GET", labelTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.labelHits))
}

// A write through the mirror flushes BEFORE forwarding, so a caller can never
// read back its own stale label in the seconds before the delivery lands.
func TestCachedLabel_WriteFlushesBeforeForwarding(t *testing.T) {
	for _, method := range []string{"PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			router, _, _, _ := respCacheStack(t)

			require.Equal(t, "miss", do(t, router, authedReq("GET", labelTarget, nil)).Header().Get(cacheHeader))
			require.Equal(t, "hit", do(t, router, authedReq("GET", labelTarget, nil)).Header().Get(cacheHeader))

			wr := do(t, router, authedReq(method, labelTarget, strings.NewReader(`{"color":"000000"}`)))
			require.Less(t, wr.Code, 300)
			assert.Empty(t, wr.Header().Get(cacheHeader), "a write is forwarded, never rebuilt")

			w := do(t, router, authedReq("GET", labelTarget, nil))
			assert.Equal(t, "miss", w.Header().Get(cacheHeader), method+" must flush the repo's labels")
		})
	}
}

// Shape guards: anything the route does not model forwards with a counted
// reason instead of vanishing.
func TestCachedLabel_ShapeGuardsPassThrough(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	// A query parameter (the endpoint takes none).
	w := do(t, router, authedReq("GET", labelTarget+"?per_page=1", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))

	// A non-default Accept.
	req := authedReq("GET", labelTarget, nil)
	req.Header.Set("Accept", "application/vnd.github.raw")
	w = do(t, router, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(cacheHeader))

	assert.Equal(t, int32(2), atomic.LoadInt32(&u.labelHits), "both must have reached GitHub")

	// The labels LIST is a different shape and is not claimed by this route.
	wl := do(t, router, authedReq("GET", "/repos/org1/repo1/labels", nil))
	assert.Empty(t, wl.Header().Get(cacheHeader))
}

// The absent answer is deliberately NOT a cached verdict — see the header note
// in respcache_labels.go. It must reach GitHub every time.
func TestCachedLabel_AbsentIsNeverStored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.label = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", labelTarget, nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.labelHits))
	}
}

// Rows key the VERBATIM requested name, so one label's row can never answer
// for a different name.
func TestCachedLabel_NamesAreDistinctRows(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	first := do(t, router, authedReq("GET", labelTarget, nil))
	require.Equal(t, "miss", first.Header().Get(cacheHeader))

	second := do(t, router, authedReq("GET", "/repos/org1/repo1/labels/merge-conflict", nil))
	require.Equal(t, "miss", second.Header().Get(cacheHeader), "a different name must not hit another label's row")
	assert.Contains(t, second.Body.String(), `"name":"merge-conflict"`)
	assert.Equal(t, int32(2), atomic.LoadInt32(&u.labelHits))
}
