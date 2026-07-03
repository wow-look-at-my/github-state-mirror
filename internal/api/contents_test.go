package api

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

func newContentsTestStack(t *testing.T, ghHandler http.Handler) (http.Handler, *ghdata.Store, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	store := ghdata.NewStore(db)
	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)

	ghSrv := httptest.NewServer(ghHandler)
	t.Cleanup(ghSrv.Close)
	gh := ghclient.NewWithBaseURL(ghSrv.URL)
	mgr.RegisterFetcher(freshness.Policy{Kind: syncpkg.KindRepoContents}, syncpkg.NewRepoContentsFetcher(gh, store))

	dispatcher := syncpkg.NewWebhookDispatcher(mgr, store, nil)
	checker := syncpkg.NewConsistencyChecker(gh, store, nil)
	router := NewRouter(mgr, store, testWebhookSecret, dispatcher, gh, []string{"*"}, testAuth(), "", checker)
	return router, store, db
}

func TestRepoContents_CachesNormalizedBodyByPathAndQuery(t *testing.T) {
	const mainBody = `{"name":"README","path":"docs/README.md","sha":"sha-main","size":13,"url":"https://api.github.test/repos/o/r/contents/docs/README.md?ref=main","html_url":"https://github.test/o/r/blob/main/docs/README.md","git_url":"https://api.github.test/repos/o/r/git/blobs/sha-main","download_url":"https://raw.githubusercontent.test/o/r/main/docs/README.md","type":"file","content":"SGVsbG8gV29ybGQhCg==\n","encoding":"base64","_links":{"self":"https://api.github.test/repos/o/r/contents/docs/README.md?ref=main","git":"https://api.github.test/repos/o/r/git/blobs/sha-main","html":"https://github.test/o/r/blob/main/docs/README.md"}}`
	const devBody = `{"name":"README","path":"docs/README.md","sha":"sha-dev","type":"file","content":"RGV2Cg==\n","encoding":"base64","_links":{"self":"https://api.github.test/repos/o/r/contents/docs/README.md?ref=dev","git":"https://api.github.test/repos/o/r/git/blobs/sha-dev","html":"https://github.test/o/r/blob/dev/docs/README.md"}}`

	var contentHits int32
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "testuser"})
		case "/repos/o/r/contents/docs/README.md":
			atomic.AddInt32(&contentHits, 1)
			assert.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			switch r.URL.RawQuery {
			case "ref=main":
				_, _ = io.WriteString(w, mainBody)
			case "ref=dev":
				_, _ = io.WriteString(w, devBody)
			default:
				t.Fatalf("unexpected contents query: %q", r.URL.RawQuery)
			}
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	})
	router, _, _ := newContentsTestStack(t, gh)

	for i := 0; i < 2; i++ {
		req := authedReq("GET", "/repos/o/r/contents/docs/README.md?ref=main", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
		assertURLStrippedJSON(t, mainBody, w.Body.String())
	}

	req := authedReq("GET", "/repos/o/r/contents/docs/README.md?ref=dev", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assertURLStrippedJSON(t, devBody, w.Body.String())

	assert.Equal(t, int32(2), atomic.LoadInt32(&contentHits),
		"same path/query should hit cache, different raw query should fetch separately")
}

func TestRepoContents_CredentialIsolation(t *testing.T) {
	var contentHits int32
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "same-login"})
		case "/repos/o/r/contents/README.md":
			atomic.AddInt32(&contentHits, 1)
			w.Header().Set("Content-Type", "application/json")
			switch r.Header.Get("Authorization") {
			case "Bearer " + testToken:
				_, _ = io.WriteString(w, `{"sha":"token-a","type":"file","content":"QQ==\n","encoding":"base64","_links":{"self":"a","git":"a","html":"a"}}`)
			case "Bearer other-token":
				_, _ = io.WriteString(w, `{"sha":"token-b","type":"file","content":"Qg==\n","encoding":"base64","_links":{"self":"b","git":"b","html":"b"}}`)
			default:
				t.Fatalf("unexpected auth: %q", r.Header.Get("Authorization"))
			}
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	})
	router, _, _ := newContentsTestStack(t, gh)

	req := authedReq("GET", "/repos/o/r/contents/README.md?ref=main", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"sha":"token-a"`)

	req = httptest.NewRequest("GET", "/repos/o/r/contents/README.md?ref=main", nil)
	req.Header.Set("Authorization", "Bearer other-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"sha":"token-b"`)

	req = authedReq("GET", "/repos/o/r/contents/README.md?ref=main", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"sha":"token-a"`)

	assert.Equal(t, int32(2), atomic.LoadInt32(&contentHits),
		"same path/query must cache separately for each actor")
}

func TestRepoContents_MediaVariantsPassthrough(t *testing.T) {
	var gotAccept string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "testuser"})
		case "/repos/o/r/contents/README.md":
			gotAccept = r.Header.Get("Accept")
			_, _ = io.WriteString(w, "raw")
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	})
	router, _, _ := newContentsTestStack(t, gh)

	req := authedReq("GET", "/repos/o/r/contents/README.md", nil)
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "raw", w.Body.String())
	assert.Equal(t, "application/vnd.github.raw+json", gotAccept)
}
