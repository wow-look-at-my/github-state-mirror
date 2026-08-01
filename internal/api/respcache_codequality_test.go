package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Code Quality setup route tests. The response body mirrors GitHub's
// documented `code-quality-setup` schema (OpenAPI, public preview), including
// its official example's null runner_label.

func defaultCodeQualityUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{
		"state": "configured",
		"languages": ["javascript-typescript", "python"],
		"runner_type": "standard",
		"runner_label": null,
		"updated_at": "2023-01-01T00:00:00Z",
		"schedule": "weekly",
		"ai_findings_option": "on_push"
	}`))
}

const codeQualityTarget = "/repos/org1/repo1/code-quality/setup"

func TestCachedCodeQualitySetup_MissAbsorbHit(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	w1 := do(t, router, authedReq("GET", codeQualityTarget, nil))
	require.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.codeQualityHits))
	assertNoURLKeys(t, w1.Body.Bytes())
	// Every documented field, with the nullables keyed rather than dropped.
	assert.JSONEq(t, `{
		"state": "configured",
		"languages": ["javascript-typescript", "python"],
		"runner_type": "standard",
		"runner_label": null,
		"updated_at": "2023-01-01T00:00:00Z",
		"schedule": "weekly",
		"ai_findings_option": "on_push"
	}`, w1.Body.String())

	w2 := do(t, router, authedReq("GET", codeQualityTarget, nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.codeQualityHits), "a hit must not call upstream")
}

// The not-configured answer is a real, cacheable verdict — it is exactly what
// a fleet enrolment sweep is asking for.
func TestCachedCodeQualitySetup_NotConfiguredIsCacheable(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.codeQuality = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"state":"not-configured","languages":[],"runner_type":null,
			"runner_label":null,"updated_at":null,"schedule":null,"ai_findings_option":null}`))
	}

	w := do(t, router, authedReq("GET", codeQualityTarget, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"state":"not-configured","languages":[],"runner_type":null,
		"runner_label":null,"updated_at":null,"schedule":null,"ai_findings_option":null}`, w.Body.String())
	assert.Equal(t, "hit", do(t, router, authedReq("GET", codeQualityTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(&u.codeQualityHits))
}

// The PATCH is the ONLY change signal the mirror can see, so proxying one must
// drop the row — otherwise a caller reads its own stale config for the TTL.
func TestCachedCodeQualitySetup_ProxiedWriteFlushes(t *testing.T) {
	router, _, _, u := respCacheStack(t)

	do(t, router, authedReq("GET", codeQualityTarget, nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", codeQualityTarget, nil)).Header().Get(cacheHeader))
	require.Equal(t, int32(1), atomic.LoadInt32(&u.codeQualityHits))

	req := authedReq("PATCH", codeQualityTarget, strings.NewReader(`{"state":"not-configured"}`))
	wp := do(t, router, req)
	require.Less(t, wp.Code, 500)
	assert.Empty(t, wp.Header().Get(cacheHeader), "a write is proxied, never served from cache")

	assert.Equal(t, "miss", do(t, router, authedReq("GET", codeQualityTarget, nil)).Header().Get(cacheHeader),
		"a proxied PATCH must flush the row")
}

// A repository event (rename/visibility/delete) flushes it repo-wide.
func TestCachedCodeQualitySetup_RepositoryEventFlushes(t *testing.T) {
	router, _, _, _ := respCacheStack(t)

	do(t, router, authedReq("GET", codeQualityTarget, nil))
	require.Equal(t, "hit", do(t, router, authedReq("GET", codeQualityTarget, nil)).Header().Get(cacheHeader))

	postWebhook(t, router, "repository", `{"action":"edited","repository":{"name":"repo1","owner":{"login":"org1"}}}`)

	assert.Equal(t, "miss", do(t, router, authedReq("GET", codeQualityTarget, nil)).Header().Get(cacheHeader))
}

// A 403 ("not available for this caller") is relayed and never stored: nothing
// observable would tell us when the feature becomes available.
func TestCachedCodeQualitySetup_NonOKRelayedUnstored(t *testing.T) {
	router, _, _, u := respCacheStack(t)
	u.codeQuality = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Code Quality is not available for this repository"}`))
	}

	for i := 1; i <= 2; i++ {
		w := do(t, router, authedReq("GET", codeQualityTarget, nil))
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader))
		assert.Equal(t, int32(i), atomic.LoadInt32(&u.codeQualityHits), "a 403 must never be answered from cache")
	}
}

func TestCachedCodeQualitySetup_ShapeGuardsPassThrough(t *testing.T) {
	router, _, _, _ := respCacheStack(t)

	w := do(t, router, authedReq("GET", codeQualityTarget+"?x=1", nil))
	assert.Empty(t, w.Header().Get(cacheHeader), "the endpoint takes no parameters, so any is unmodeled")

	req := httptest.NewRequest("GET", codeQualityTarget, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Accept", "application/vnd.github.raw")
	assert.Empty(t, do(t, router, req).Header().Get(cacheHeader))
}
