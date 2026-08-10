package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mutableRefTTLCeiling is the bound these tests hold the cache to, stated
// INDEPENDENTLY of mutableRefCacheTTL: a row keyed by a mutable ref must
// expire within minutes, whatever that constant happens to say. Asserting
// against the constant would be a tautology that passes at any value,
// including the day-long one that let one lost push serve a wrong branch tip
// until the row aged out.
const mutableRefTTLCeiling = 15 * time.Minute

// The TTL a stored comparison gets is chosen from the SHAPE of its key. A
// basehead naming a branch describes an answer that moves whenever that branch
// moves, and the push flush is its only freshness signal -- so a lost delivery
// pins it for the whole window. A sha...sha basehead describes two immutable
// commits and can hold for a day.
func TestCachedCompare_TTLByKeyShape(t *testing.T) {
	for _, tc := range []struct {
		name     string
		basehead string
		mutable  bool
	}{
		{"branch...branch", "main...dev", true},
		{"branch...sha", "main..." + shaTip, true},
		{"sha...branch", shaBase + "...dev", true},
		{"sha...sha", shaBase + "..." + shaTip, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, _, db, _ := compareCacheStack(t)

			w := do(t, router, authedReq("GET", "/repos/org1/repo1/compare/"+tc.basehead, nil))
			require.Equal(t, http.StatusOK, w.Code)

			var raw string
			require.NoError(t, db.QueryRow(`SELECT expires_at FROM compare_cache`).Scan(&raw))
			exp, err := time.Parse(time.RFC3339, raw)
			require.NoError(t, err)

			ceiling := time.Now().UTC().Add(mutableRefTTLCeiling)
			if tc.mutable {
				assert.True(t, exp.Before(ceiling),
					"basehead %q names a mutable ref: it must expire within %s, got %s", tc.basehead, mutableRefTTLCeiling, raw)
			} else {
				assert.True(t, exp.After(ceiling),
					"basehead %q is sha...sha and immutable: it must outlive the mutable window, got %s", tc.basehead, raw)
			}
			assert.True(t, exp.After(time.Now().UTC()), "a freshly stored row must not already be expired")
		})
	}
}

// compareTTLFor's SELECTION rule, independent of either constant's value: only
// a full-sha basehead on both sides earns the long window.
func TestCompareTTLFor(t *testing.T) {
	for _, tc := range []struct {
		base, head string
	}{
		{"main", "dev"},
		{"main", shaTip},
		{shaBase, "dev"},
		// An abbreviated sha is immutable in fact, but it is not a shape we
		// prove, and a wrong "immutable" call costs a day of stale answers.
		{shaBase[:12], shaTip[:12]},
		// A 40-character non-hex ref name must never read as a sha.
		{strings.Repeat("z", 40), shaTip},
	} {
		assert.LessOrEqual(t, compareTTLFor(tc.base, tc.head), mutableRefTTLCeiling,
			"%s...%s names a mutable ref", tc.base, tc.head)
	}

	assert.Greater(t, compareTTLFor(shaBase, shaTip), mutableRefTTLCeiling,
		"sha...sha is immutable and may hold far longer")
	assert.Equal(t, compareTTLFor(shaBase, shaTip), compareTTLFor(strings.ToUpper(shaBase), strings.ToUpper(shaTip)),
		"hex case must not change the verdict")
}
