package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// checkerFakeGitHub serves the consistency checker's whole fetch surface for
// one owner "org1" with one live repo: App installations, token mints, and
// /graphql answering both owner-agnostic queries (data + visibility twins).
func checkerFakeGitHub(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "account": map[string]any{"login": "org1", "type": "Organization"}},
		})
	})
	mux.HandleFunc("/app/installations/1/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_org1"})
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		nodes := []map[string]any{{
			"name": "repo1", "nameWithOwner": "org1/repo1", "url": "https://github.com/org1/repo1",
			"isDisabled": false, "isArchived": false, "pushedAt": "2024-01-01T00:00:00Z",
			"owner": map[string]string{"login": "org1", "avatarUrl": "a", "url": "u"},
			"defaultBranchRef": map[string]any{
				"name":   "main",
				"target": map[string]any{"statusCheckRollup": map[string]string{"state": "SUCCESS"}},
			},
			"pullRequests": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
				"nodes":    []map[string]any{},
			},
		}}
		if !strings.Contains(req.Query, "pullRequests") { // the visibility twin
			nodes = []map[string]any{{"name": "repo1", "visibility": "PUBLIC", "isArchived": false}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"repositoryOwner": map[string]any{
					"repositories": map[string]any{
						"totalCount": len(nodes),
						"pageInfo":   map[string]any{"hasNextPage": false, "endCursor": ""},
						"nodes":      nodes,
					},
				},
			},
		})
	})
	return mux
}

// newCheckerStack builds the router with an APP-ENABLED consistency checker
// pointed at the given fake GitHub (unlike newTestStackWithGitHub, whose
// checker has a nil app and reports unavailable).
func newCheckerStack(t *testing.T, authSvc *auth.Service, gh http.Handler) (http.Handler, *ghdata.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	store := ghdata.NewStore(db)
	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	mgr.RegisterFetcher(freshness.Policy{Kind: syncpkg.KindOrgRepos}, &stubFetcher{})
	dispatcher := syncpkg.NewWebhookDispatcher(mgr, store)

	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)
	client := ghclient.NewWithBaseURL(ghSrv.URL)
	meter := ratemeter.New()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	app, err := ghclient.NewAppAuthenticator("42", keyPEM, client)
	require.NoError(t, err)

	checker := syncpkg.NewConsistencyChecker(client, store, fStore, app)
	return NewRouter(mgr, store, testWebhookSecret, dispatcher, client, []string{"*"}, authSvc, "", checker, meter), store
}

// decodeStreamLines splits an NDJSON body and unmarshals every line.
func decodeStreamLines(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		require.NotEmpty(t, line, "no blank lines in the NDJSON stream")
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "every stream line must be standalone JSON: %q", line)
		out = append(out, m)
	}
	return out
}

// TestCacheCheckStream: ?stream=1 answers NDJSON -- one line per progress
// event (start/owner/fetch/... phases in order) with the full report as the
// final line, equal in content to what the non-stream path returns.
func TestCacheCheckStream(t *testing.T) {
	svc := configuredAuth(t)
	router, store := newCheckerStack(t, svc, checkerFakeGitHub(t))
	// One cached repo so org1 is an owner to check (drift-free vs the fake).
	require.NoError(t, store.UpsertRepo(context.Background(), dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
	}))

	req := httptest.NewRequest("GET", "/api/cache/check?stream=1", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/x-ndjson", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	lines := decodeStreamLines(t, w.Body.String())
	require.GreaterOrEqual(t, len(lines), 5, "start + owner + fetch + ... + report")

	var phases []string
	for _, l := range lines {
		phase, _ := l["phase"].(string)
		require.NotEmpty(t, phase, "every line carries a phase")
		phases = append(phases, phase)
	}
	assert.Equal(t, "start", phases[0])
	assert.Contains(t, phases, "owner")
	assert.Contains(t, phases, "fetch")
	assert.Contains(t, phases, "visibility")
	assert.Contains(t, phases, "diffed")
	assert.Equal(t, "done", phases[len(phases)-2])
	assert.Equal(t, "report", phases[len(phases)-1])

	// The final line's report is the same report the non-stream path returns.
	reportJSON, err := json.Marshal(lines[len(lines)-1]["report"])
	require.NoError(t, err)
	var streamed syncpkg.ConsistencyReport
	require.NoError(t, json.Unmarshal(reportJSON, &streamed))

	req = httptest.NewRequest("GET", "/api/cache/check", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"), "the non-stream path is unchanged")
	var plain syncpkg.ConsistencyReport
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &plain), "the non-stream body stays ONE buffered JSON report")

	assert.Equal(t, plain.OrgsChecked, streamed.OrgsChecked)
	assert.Equal(t, plain.Summary, streamed.Summary)
	assert.Len(t, streamed.Discrepancies, len(plain.Discrepancies))
	assert.Nil(t, streamed.Applied)
}

// TestCacheCheckStream_Apply: the reconcile (POST ?apply=true&stream=1)
// streams too, including per-owner "applied" tally events, and the final
// report carries the applied summary.
func TestCacheCheckStream_Apply(t *testing.T) {
	svc := configuredAuth(t)
	router, store := newCheckerStack(t, svc, checkerFakeGitHub(t))
	require.NoError(t, store.UpsertRepo(context.Background(), dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
	}))

	req := httptest.NewRequest("POST", "/api/cache/check?stream=1&apply=true", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	lines := decodeStreamLines(t, w.Body.String())
	var sawApplied bool
	for _, l := range lines {
		if l["phase"] == "applied" {
			sawApplied = true
			assert.Equal(t, "org1", l["owner"])
			assert.NotNil(t, l["applied"], "the applied event carries the tally snapshot")
		}
	}
	assert.True(t, sawApplied, "apply mode must emit per-owner applied events")

	last := lines[len(lines)-1]
	require.Equal(t, "report", last["phase"])
	report, ok := last["report"].(map[string]any)
	require.True(t, ok)
	assert.NotNil(t, report["applied"], "the streamed report is the apply report")
}

// TestCacheCheckStream_RunError: a run failing after the stream opened (here:
// the installations listing 500s) surfaces as a terminal {"phase":"error"}
// line -- the 200 is already committed, so the line IS the error channel.
func TestCacheCheckStream_RunError(t *testing.T) {
	svc := configuredAuth(t)
	gh := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	router, store := newCheckerStack(t, svc, gh)
	require.NoError(t, store.UpsertRepo(context.Background(), dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u",
	}))

	req := httptest.NewRequest("GET", "/api/cache/check?stream=1", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	lines := decodeStreamLines(t, w.Body.String())
	last := lines[len(lines)-1]
	assert.Equal(t, "error", last["phase"])
	errMsg, _ := last["error"].(string)
	assert.Contains(t, errMsg, "consistency check failed")
}

// TestCacheCheckStream_GatesStillApply: stream=1 changes the response format
// only -- the admin gate, the apply-needs-POST gate, and the no-App 503 all
// fire before any streaming starts.
func TestCacheCheckStream_GatesStillApply(t *testing.T) {
	svc := configuredAuth(t)

	// No GitHub App (the plain test stack): 503, not a stream.
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/api/cache/check?stream=1", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	// Apply on a GET: still 405.
	req = httptest.NewRequest("GET", "/api/cache/check?stream=1&apply=true", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)

	// Non-admin: still 403.
	req = httptest.NewRequest("GET", "/api/cache/check?stream=1", nil)
	req.AddCookie(mintSession(t, svc, "octocat"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}
