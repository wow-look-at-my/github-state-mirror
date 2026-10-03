package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingRelayer struct {
	mu     sync.Mutex
	bodies []string
}

func (r *recordingRelayer) RelayDelivery(_ http.Header, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, string(body))
}

func (r *recordingRelayer) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

func TestHandlerWithRelay_RelaysOnlyVerifiedDeliveries(t *testing.T) {
	const secret = "test-secret"
	body := `{"action":"opened"}`
	for _, tc := range []struct {
		name    string
		sig     string
		secret  string
		relayed bool
	}{
		{"verified", signPayload(secret, []byte(body)), secret, true},
		{"bad signature", "sha256=deadbeef", secret, false},
		{"no signature", "", secret, false},
		{"no secret configured", signPayload(secret, []byte(body)), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := &recordingRelayer{}
			// An error disposition still relays: the relay does not depend on the dispatch outcome.
			dispatcher := &recordingDispatcher{result: DispatchResult{Disposition: DispError}}
			handler := HandlerWithRelay(tc.secret, dispatcher, nil, rel)

			req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
			if tc.sig != "" {
				req.Header.Set("X-Hub-Signature-256", tc.sig)
			}
			req.Header.Set("X-GitHub-Event", "push")
			handler.ServeHTTP(httptest.NewRecorder(), req)

			if tc.relayed {
				require.Equal(t, []string{body}, rel.got())
			} else {
				assert.Empty(t, rel.got(), "an unverified delivery must never be relayed")
			}
		})
	}
}
