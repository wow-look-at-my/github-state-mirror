// Covers bearerToken: the token requireAuth reads from the Authorization header.

package api

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBearerToken_SchemeIsCaseInsensitive(t *testing.T) {
	const jwtShaped = "ghs_123_eyJh.eyJi.c2ln"
	for header, want := range map[string]string{
		"Bearer " + jwtShaped: jwtShaped,
		"bearer " + jwtShaped: jwtShaped,
		"token ghs_plain":     "ghs_plain",
		"Token ghs_plain":     "ghs_plain",
		"ghs_bare":            "ghs_bare",
		"":                    "",
	} {
		r := httptest.NewRequest("GET", "/user", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		assert.Equal(t, want, bearerToken(r), "Authorization %q", header)
	}
}
