// Package httpobs makes an outbound HTTP client observable at the place
// that cannot be bypassed: its transport.
//
// Every request this service sends has to be visible on the dashboard. A
// request nobody can see is a request nobody can account for -- it spends
// rate-limit budget, it can be slow, it can fail, and none of that reaches
// the operator looking at the chart to answer "what is this thing doing?".
//
// Instrumenting call sites does not achieve that, because the property is
// about the ones nobody remembered. A client wrapped here reports every real
// attempt it makes, through any helper, present or future -- which is the
// same reason ghclient times its own exchanges at the transport rather than
// in each method.
//
// Leaf package: stdlib only, so anything may import it (the internal/actor
// stance). What an observation MEANS -- which lane, which identity -- is the
// caller's to decide.
package httpobs

import (
	"net/http"
	"time"
)

// Observer receives every attempt the wrapped client makes, with its REAL
type Observer func(req *http.Request, status int, start time.Time, dur time.Duration)

// Transport wraps base so every round trip is reported to obs. A nil base
// means http.DefaultTransport. A nil obs returns base unchanged -- the
func Transport(base http.RoundTripper, obs Observer) http.RoundTripper {
	if obs == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &timingTransport{base: base, observe: obs}
}

// Client returns an HTTP client whose every request is reported to obs.
func Client(timeout time.Duration, obs Observer) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport(nil, obs)}
}

type timingTransport struct {
	base    http.RoundTripper
	observe Observer
}

// RoundTrip times the exchange and reports it. It never alters the request or
// the response, and never swallows an error: an observer is a bystander.
func (t *timingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	t.observe(req, status, start, time.Since(start))
	return resp, err
}
