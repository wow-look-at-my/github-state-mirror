package api

import (
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Log redirect tests.

const (
	runLogsTarget = "/repos/org1/repo1/actions/runs/4242/logs"
	jobLogsTarget = "/repos/org1/repo1/actions/jobs/77/logs"
)

func sasLocation(expires time.Time) string {
	q := url.Values{"sv": {"2023-11-03"}, "se": {expires.UTC().Format(time.RFC3339)}, "sig": {"abc"}}
	return "https://results.blob.core.windows.net/logs/77.txt?" + q.Encode()
}

// logsStack answers both log routes with status and location, counting calls.
func logsStack(t *testing.T) (http.Handler, *int32, *int, *string) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var calls int32
	status := http.StatusFound
	location := sasLocation(time.Now().Add(time.Minute))
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/logs") {
			return false
		}
		atomic.AddInt32(&calls, 1)
		if status == http.StatusFound {
			w.Header().Set("Location", location)
			w.WriteHeader(status)
			return true
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"Logs are gone","status":"410"}`))
		return true
	}
	return router, &calls, &status, &location
}

func TestCachedLogs_RedirectStoredUntilItsExpiry(t *testing.T) {
	router, calls, _, location := logsStack(t)

	for _, target := range []string{runLogsTarget, jobLogsTarget} {
		for i, want := range []string{"miss", "hit"} {
			w := do(t, router, authedReq("GET", target, nil))
			require.Equal(t, http.StatusFound, w.Code, "%s request %d", target, i)
			assert.Equal(t, want, w.Header().Get(cacheHeader), "%s request %d", target, i)
			assert.Equal(t, *location, w.Header().Get("Location"))
			assert.Empty(t, w.Body.String())
		}
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(calls), "the mirror must not follow the redirect")
}

// A location inside the safety margin, or with no expiry the route can read,
// relays unstored.
func TestCachedLogs_UnreadableOrNearExpiryRelays(t *testing.T) {
	router, calls, _, location := logsStack(t)
	for i, loc := range []string{
		sasLocation(time.Now().Add(5 * time.Second)),
		"https://results.blob.core.windows.net/logs/77.txt?sig=abc",
	} {
		*location = loc
		for j := 0; j < 2; j++ {
			w := do(t, router, authedReq("GET", jobLogsTarget, nil))
			require.Equal(t, http.StatusFound, w.Code)
			assert.Empty(t, w.Header().Get(cacheHeader), "location %d", i)
			assert.Equal(t, loc, w.Header().Get("Location"))
		}
	}
	assert.Equal(t, int32(4), atomic.LoadInt32(calls))
}

func TestCachedLogs_Verdicts(t *testing.T) {
	router, calls, status, _ := logsStack(t)
	for _, tc := range []struct {
		status int
		target string
		stored bool
	}{
		{http.StatusGone, jobLogsTarget, true},
		{http.StatusNotFound, runLogsTarget, true},
		{http.StatusUnsupportedMediaType, "/repos/org1/repo1/actions/jobs/78/logs", false},
	} {
		*status = tc.status
		before := atomic.LoadInt32(calls)
		do(t, router, authedReq("GET", tc.target, nil))
		w := do(t, router, authedReq("GET", tc.target, nil))
		require.Equal(t, tc.status, w.Code)
		if tc.stored {
			assert.Equal(t, "hit", w.Header().Get(cacheHeader), "status %d", tc.status)
			assert.Equal(t, before+1, atomic.LoadInt32(calls))
		} else {
			assert.Empty(t, w.Header().Get(cacheHeader), "status %d", tc.status)
			assert.Equal(t, before+2, atomic.LoadInt32(calls))
		}
	}
}

func TestCachedLogs_ShapeGuardsPassThrough(t *testing.T) {
	router, calls, _, _ := logsStack(t)
	req := authedReq("GET", jobLogsTarget, nil)
	req.Header.Set("Accept", "text/plain")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
	assert.Empty(t, do(t, router, authedReq("GET", jobLogsTarget+"?x=1", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

func TestSignedURLExpiry(t *testing.T) {
	at := time.Date(2026, 10, 12, 5, 0, 0, 0, time.UTC)
	for loc, want := range map[string]time.Time{
		sasLocation(at): at,
		"https://s3.example.com/x?X-Amz-Date=20261012T050000Z&X-Amz-Expires=300": at.Add(5 * time.Minute),
		"https://cdn.example.com/x?Expires=1791781200":                           time.Unix(1791781200, 0),
	} {
		got, ok := signedURLExpiry(loc)
		require.True(t, ok, loc)
		assert.True(t, want.Equal(got), "%s: got %s", loc, got)
	}
	for _, loc := range []string{"", "/relative?se=2026-10-12T05:00:00Z", "https://x.example.com/x?token=abc", "https://x.example.com/x?se=soon"} {
		_, ok := signedURLExpiry(loc)
		assert.False(t, ok, loc)
	}
}
