package api

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Org custom property values tests.

const propertyValuesTarget = "/orgs/org1/properties/values"

// propertyValuesStack answers the org property values read, counting calls.
func propertyValuesStack(t *testing.T) (http.Handler, *int32, *int, *string) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var calls int32
	status := http.StatusOK
	env := "production"
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != propertyValuesTarget {
			return false
		}
		atomic.AddInt32(&calls, 1)
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com","status":"403"}`))
			return true
		}
		writeGitHubJSON(w, []any{map[string]any{
			"repository_id": 42, "repository_name": "repo1", "repository_full_name": "org1/repo1",
			"properties": []any{
				map[string]any{"property_name": "environment", "value": env},
				map[string]any{"property_name": "teams", "value": []any{"a", "b"}},
			},
		}})
		return true
	}
	return router, &calls, &status, &env
}

func TestCachedOrgPropertyValues_MissAbsorbHit(t *testing.T) {
	router, calls, _, _ := propertyValuesStack(t)

	w1 := do(t, router, authedReq("GET", propertyValuesTarget+"?per_page=100&page=1", nil))
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	assertNoURLKeys(t, w1.Body.Bytes())
	assert.Contains(t, w1.Body.String(), `"value":["a","b"]`)

	w2 := do(t, router, authedReq("GET", propertyValuesTarget+"?page=1&per_page=100", nil))
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, w1.Body.String(), w2.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

func TestCachedOrgPropertyValues_NeverSharedAcrossCredentials(t *testing.T) {
	router, calls, _, _ := propertyValuesStack(t)

	require.Equal(t, "miss", do(t, router, hooksReq("GET", propertyValuesTarget, "tok-a")).Header().Get(cacheHeader))
	assert.Equal(t, "miss", do(t, router, hooksReq("GET", propertyValuesTarget, "tok-b")).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

func TestCachedOrgPropertyValues_ForbiddenStoredPerCredential(t *testing.T) {
	router, calls, status, _ := propertyValuesStack(t)
	*status = http.StatusForbidden

	w1 := do(t, router, hooksReq("GET", propertyValuesTarget, "tok-a"))
	require.Equal(t, http.StatusForbidden, w1.Code)
	assert.Equal(t, "miss", w1.Header().Get(cacheHeader))
	w2 := do(t, router, hooksReq("GET", propertyValuesTarget, "tok-a"))
	require.Equal(t, http.StatusForbidden, w2.Code)
	assert.Equal(t, "hit", w2.Header().Get(cacheHeader))
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

func TestCachedOrgPropertyValues_ShapeGuards(t *testing.T) {
	router, calls, _, _ := propertyValuesStack(t)

	assert.Empty(t, do(t, router, authedReq("GET", propertyValuesTarget+"?repository_query=x", nil)).Header().Get(cacheHeader))
	assert.Empty(t, do(t, router, authedReq("GET", propertyValuesTarget+"?page=11", nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

// A changed value of a listed property is written into the stored page.
func TestCachedOrgPropertyValues_DeliveryAppliesInPlace(t *testing.T) {
	router, calls, _, env := propertyValuesStack(t)
	require.Equal(t, "miss", do(t, router, authedReq("GET", propertyValuesTarget, nil)).Header().Get(cacheHeader))

	*env = "staging"
	repo := fixtureRepo()
	repo["id"] = 42
	postWebhookJSON(t, router, "custom_property_values", map[string]any{
		"action": "updated", "repository": repo, "organization": map[string]any{"login": "org1"},
		"new_property_values": []any{map[string]any{"property_name": "environment", "value": "staging"}},
	})
	w := do(t, router, authedReq("GET", propertyValuesTarget, nil))
	assert.Equal(t, "hit", w.Header().Get(cacheHeader))
	assert.Contains(t, w.Body.String(), `"value":"staging"`)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	// The applied page must match what a fetch now answers.
	fresh := do(t, router, hooksReq("GET", propertyValuesTarget, "tok-other"))
	assert.Equal(t, fresh.Body.String(), w.Body.String())
}

// A value that became null cannot be written in place, so the page drops.
func TestCachedOrgPropertyValues_NullValueFlushes(t *testing.T) {
	router, calls, _, _ := propertyValuesStack(t)
	require.Equal(t, "miss", do(t, router, authedReq("GET", propertyValuesTarget, nil)).Header().Get(cacheHeader))

	repo := fixtureRepo()
	repo["id"] = 42
	postWebhookJSON(t, router, "custom_property_values", map[string]any{
		"action": "updated", "repository": repo, "organization": map[string]any{"login": "org1"},
		"new_property_values": []any{map[string]any{"property_name": "environment", "value": nil}},
	})
	assert.Equal(t, "miss", do(t, router, authedReq("GET", propertyValuesTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

func TestCachedOrgPropertyValues_DefinitionChangeFlushes(t *testing.T) {
	router, calls, _, _ := propertyValuesStack(t)
	require.Equal(t, "miss", do(t, router, authedReq("GET", propertyValuesTarget, nil)).Header().Get(cacheHeader))

	postWebhookJSON(t, router, "custom_property", map[string]any{
		"action": "created", "organization": map[string]any{"login": "org1"},
		"definition": map[string]any{"property_name": "tier"},
	})
	assert.Equal(t, "miss", do(t, router, authedReq("GET", propertyValuesTarget, nil)).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}
