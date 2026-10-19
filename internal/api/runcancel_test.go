package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
)

// upstreamLog records every request the fake GitHub receives.
type upstreamLog struct {
	mu    sync.Mutex
	calls []string
}

func (u *upstreamLog) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls = append(u.calls, r.Method+" "+r.URL.Path)
		u.mu.Unlock()
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			writeGitHubJSON(w, map[string]any{})
			return
		}
		writeGitHubJSON(w, map[string]any{"login": testUserLogin, "id": testUserID})
	})
}

func (u *upstreamLog) snapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.calls...)
}

func runCancelStack(t *testing.T) (http.Handler, *upstreamLog) {
	t.Helper()
	u := &upstreamLog{}
	router, _, _, _ := newTestStackWithGitHub(t, auth.New(auth.Config{SessionKey: []byte("test-session-key")}), u.handler())
	return router, u
}

func TestRefuseRunCancel_EverySpelling(t *testing.T) {
	router, u := runCancelStack(t)
	targets := []string{
		"/repos/o/r/actions/runs/42/cancel",
		"/repos/o/r/actions/runs/42/force-cancel",
		"/api/v3/repos/o/r/actions/runs/42/cancel",
		"/api/v3/repos/o/r/actions/runs/42/force-cancel",
		"/repos/o/r/actions/runs/42/cancel/",
		"/repos/o/r/actions/runs/42//force-cancel",
		"/repos/o/r/actions/runs/42/x/../cancel",
		"/repos/o/r/actions/runs/42/%63ancel",
		"/api/v3/repos/o/r/Actions/Runs/42/CANCEL",
	}
	for _, target := range targets {
		w := do(t, router, authedReq(http.MethodPost, target, nil))
		require.Equal(t, http.StatusForbidden, w.Code, "POST %s", target)
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"), "POST %s", target)
		var body runCancelRefusal
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "POST %s", target)
		assert.Equal(t, runCancelMessage, body.Message, "POST %s", target)
		assert.Equal(t, "403", body.Status, "POST %s", target)
	}
	for _, call := range u.snapshot() {
		assert.NotContains(t, call, "cancel", "no cancel may reach GitHub")
		assert.NotContains(t, call, "CANCEL", "no cancel may reach GitHub")
	}
}

func TestRefuseRunCancel_NoTokenStillRefused(t *testing.T) {
	router, u := runCancelStack(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/repos/o/r/actions/runs/42/cancel", nil))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, u.snapshot(), "a refused cancel makes no upstream call")
}

// TestRefuseRunCancel_RerunStillProxies: the neighbouring run writes are
// untouched and reach GitHub on their exact path.
func TestRefuseRunCancel_RerunStillProxies(t *testing.T) {
	router, u := runCancelStack(t)
	for _, target := range []string{
		"/repos/o/r/actions/runs/42/rerun",
		"/repos/o/r/actions/runs/42/rerun-failed-jobs",
		"/api/v3/repos/o/r/actions/jobs/7/rerun",
	} {
		w := do(t, router, authedReq(http.MethodPost, target, nil))
		assert.Equal(t, http.StatusCreated, w.Code, "POST %s must pass GitHub's answer back", target)
	}
	calls := u.snapshot()
	assert.Contains(t, calls, "POST /repos/o/r/actions/runs/42/rerun")
	assert.Contains(t, calls, "POST /repos/o/r/actions/runs/42/rerun-failed-jobs")
	assert.Contains(t, calls, "POST /repos/o/r/actions/jobs/7/rerun")
}

func TestIsRunCancelPath(t *testing.T) {
	cases := map[string]bool{
		"/repos/o/r/actions/runs/1/cancel":       true,
		"/repos/o/r/actions/runs/1/force-cancel": true,
		"/repos/o/r/actions/runs/1/rerun":        false,
		"/repos/o/r/actions/runs/1":              false,
		"/repos/o/r/actions/runs/1/cancel/extra": false,
		"/repos/o/r/actions/jobs/1/cancel":       false,
		"/repos/o/r/pages/deployments/1/cancel":  false,
		"/repos/o/r/actions/runs/cancel":         false,
	}
	for p, want := range cases {
		assert.Equal(t, want, isRunCancelPath(p), p)
	}
}
