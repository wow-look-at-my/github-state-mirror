package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Archive redirect tests. The ref carries slashes, as a tag like cmd/v1 does.

func archiveStack(t *testing.T) (http.Handler, *int32, *string) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var calls int32
	location := "https://codeload.github.com/org1/repo1/legacy.tar.gz/refs/tags/cmd/v1.28.0"
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/tarball") && !strings.Contains(r.URL.Path, "/zipball") {
			return false
		}
		atomic.AddInt32(&calls, 1)
		if location == "" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("Bad credentials"))
			return true
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
		return true
	}
	return router, &calls, &location
}

func TestCachedArchive_UnsignedRedirectStored(t *testing.T) {
	router, calls, location := archiveStack(t)
	for _, target := range []string{"/repos/org1/repo1/tarball/cmd/v1.28.0", "/repos/org1/repo1/zipball/cmd/v1.28.0", "/repos/org1/repo1/tarball"} {
		for i, want := range []string{"miss", "hit"} {
			w := do(t, router, authedReq("GET", target, nil))
			require.Equal(t, http.StatusFound, w.Code, "%s request %d", target, i)
			assert.Equal(t, want, w.Header().Get(cacheHeader), "%s request %d", target, i)
			assert.Equal(t, *location, w.Header().Get("Location"))
		}
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(calls))
}

func TestCachedArchive_TokenAndUnauthorizedRelay(t *testing.T) {
	router, calls, location := archiveStack(t)
	// A private repo's codeload link lives a few minutes, which GitHub documents.
	*location = "https://codeload.github.com/org1/repo1/legacy.tar.gz/refs/heads/main?token=ABC"
	for i, want := range []string{"miss", "hit"} {
		w := do(t, router, authedReq("GET", "/repos/org1/repo1/tarball/main", nil))
		require.Equal(t, http.StatusFound, w.Code)
		assert.Equal(t, want, w.Header().Get(cacheHeader), "request %d", i)
		assert.Equal(t, *location, w.Header().Get("Location"))
	}
	// A credential on any other host states no life, so it relays.
	*location = "https://archive.example.com/org1/repo1.tar.gz?token=ABC"
	for i := 0; i < 2; i++ {
		w := do(t, router, authedReq("GET", "/repos/org1/repo1/tarball/dev", nil))
		require.Equal(t, http.StatusFound, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
	}
	*location = ""
	for i := 0; i < 2; i++ {
		w := do(t, router, authedReq("GET", "/repos/org1/repo1/zipball/main", nil))
		require.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
	}
	assert.Equal(t, int32(5), atomic.LoadInt32(calls))
}
