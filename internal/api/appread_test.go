package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

const (
	testFineGrainedPAT = "github_pat_test"
	testAppToken       = "ghs_app_installation"
	patRefusalBody     = `{"message":"Resource not accessible by personal access token","status":"403"}`
)

// checksGitHub is a fake GitHub whose Checks API answers the App's installation token and refuses every other token.
type checksGitHub struct {
	u *commitCIUpstream
	// installed is the accounts the App is installed for.
	installed []string

	mu sync.Mutex
	// checksAuth is the Authorization of each Checks API request, in order.
	checksAuth []string
	mints      int
}

func (g *checksGitHub) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.checksAuth...)
}

func (g *checksGitHub) checks(w http.ResponseWriter, r *http.Request, ok func()) {
	auth := r.Header.Get("Authorization")
	g.mu.Lock()
	g.checksAuth = append(g.checksAuth, auth)
	g.mu.Unlock()
	if auth != "Bearer "+testAppToken {
		writeStatus(w, http.StatusForbidden, patRefusalBody)
		return
	}
	ok()
}

func (g *checksGitHub) handler() http.Handler {
	base := g.u.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			list := []any{}
			for i, login := range g.installed {
				list = append(list, map[string]any{"id": 7 + i, "account": map[string]any{"login": login, "type": "Organization"}})
			}
			writeGitHubJSON(w, list)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			g.mu.Lock()
			g.mints++
			g.mu.Unlock()
			writeGitHubJSON(w, map[string]any{"token": testAppToken})
		case strings.HasSuffix(r.URL.Path, "/annotations"):
			g.checks(w, r, func() {
				writeGitHubJSON(w, []any{map[string]any{"path": "a.go", "start_line": 1, "end_line": 1, "annotation_level": "failure", "message": "boom"}})
			})
		case strings.Contains(r.URL.Path, "/check-runs/"):
			g.checks(w, r, func() {
				writeGitHubJSON(w, upstreamCheckRun(101, "build", "completed", "success", "2026-07-01T10:00:00Z", "2026-07-01T10:04:00Z"))
			})
		default:
			base.ServeHTTP(w, r)
		}
	})
}

// checksStack is the router with a GitHub App, over a fake GitHub that gives the Checks API to that App alone.
func checksStack(t *testing.T, installed ...string) (http.Handler, *checksGitHub) {
	t.Helper()
	g := &checksGitHub{u: newCommitCIUpstream(), installed: installed}
	inner := g.u.checkRuns
	g.u.checkRuns = func(w http.ResponseWriter, r *http.Request) { g.checks(w, r, func() { inner(w, r) }) }

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db := openTestDB(t, dbPath)
	t.Cleanup(func() { db.Close() })
	store := ghdata.NewStore(db)
	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	mgr.RegisterFetcher(freshness.Policy{Kind: syncpkg.KindOrgRepos}, &stubFetcher{})
	dispatcher := syncpkg.NewWebhookDispatcher(mgr, store)

	ghSrv := httptest.NewServer(g.handler())
	t.Cleanup(ghSrv.Close)
	client := ghclient.NewWithBaseURL(ghSrv.URL)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	app, err := ghclient.NewAppAuthenticator("42", keyPEM, client)
	require.NoError(t, err)

	checker := syncpkg.NewConsistencyChecker(client, store, fStore, app)
	queue := newTestQueue(t, dispatcher, nil)
	router := NewRouter(mgr, store, testWebhookSecret, dispatcher, queue, client, []string{"*"}, testAuth(), "", checker, ratemeter.New(), nil, dbPath, nil, nil, app, nil, nil, nil, nil)
	return router, g
}

func asToken(target, token string) *http.Request {
	req := authedReq("GET", target, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

const checkRunsTarget = "/repos/org1/repo1/commits/" + shaTip + "/check-runs?per_page=100"

// GitHub gives a fine-grained PAT no Checks scope. The App reads for a caller that can read the repository.
func TestChecks_FineGrainedPATReadsThroughTheApp(t *testing.T) {
	router, g := checksStack(t, "org1")

	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, asToken(checkRunsTarget, testFineGrainedPAT))
		require.Equal(t, http.StatusOK, w.Code, "request %d: %s", i, w.Body.String())
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
		assert.Contains(t, w.Body.String(), `"total_count":2`)
	}
	assert.Equal(t, []string{"Bearer " + testAppToken}, g.seen(), "the PAT is never sent to the Checks API, and a hit sends nothing")
	assert.Equal(t, 1, g.mints)
}

// A token with another shape that earns the same refusal is read through the App after its own attempt.
func TestChecks_ScopeRefusalFallsBackToTheApp(t *testing.T) {
	router, g := checksStack(t, "org1")

	w := do(t, router, asToken(checkRunsTarget, "some-other-token"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"Bearer some-other-token", "Bearer " + testAppToken}, g.seen())
}

// The App reads only where the caller proved read access to the repository.
func TestChecks_NoRepositoryAccessMeansNoAppRead(t *testing.T) {
	router, g := checksStack(t, "org1")
	g.u.probe = func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusNotFound, `{"message":"Not Found","status":"404"}`)
	}

	w := do(t, router, asToken(checkRunsTarget, testFineGrainedPAT))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Empty(t, g.seen(), "a caller that cannot read the repository reaches no Checks API request")
	assert.Equal(t, 0, g.mints)
}

// With the App not installed for the account, the caller's own refusal is the answer.
func TestChecks_AppNotInstalledKeepsTheRefusal(t *testing.T) {
	router, g := checksStack(t, "another-org")

	w := do(t, router, asToken(checkRunsTarget, testFineGrainedPAT))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, []string{"Bearer " + testFineGrainedPAT}, g.seen())
}

func TestChecks_SingleRunAndAnnotationsReadThroughTheApp(t *testing.T) {
	router, g := checksStack(t, "org1")

	w := do(t, router, asToken("/repos/org1/repo1/check-runs/101", testFineGrainedPAT))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"name":"build"`)

	w = do(t, router, asToken("/repos/org1/repo1/check-runs/101/annotations", testFineGrainedPAT))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "boom")

	for _, auth := range g.seen() {
		assert.Equal(t, "Bearer "+testAppToken, auth)
	}
}
