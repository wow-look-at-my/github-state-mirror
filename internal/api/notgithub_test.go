package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotGitHub_ScannerProbeAnsweredLocally(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	var calls int32
	u.extra = func(http.ResponseWriter, *http.Request) bool {
		atomic.AddInt32(&calls, 1)
		return false
	}

	w := do(t, router, httptest.NewRequest("GET", "/wp-json/gravitysmtp/v1/tests/mock-data?page=1", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.JSONEq(t, mustJSONString(map[string]any{"message": "Not Found", "status": "404"}), w.Body.String())
	assert.Zero(t, atomic.LoadInt32(&calls), "a probe must not reach GitHub")
}
