package webhook

import "net/http"

// Disposition values describe what the dispatcher did with a delivery. They are
// returned to GitHub in the HTTP response and recorded in the webhook delivery
// log shown on the dashboard.
const (
	DispApplied     = "applied"     // webhook data was written into >=1 cache scope
	DispSkipped     = "skipped"     // parsed fine, but no cache scope had this repo
	DispInvalidated = "invalidated" // marked cache stale (fallback / structural change)
	DispIgnored     = "ignored"     // an event or action the mirror does not track
	DispError       = "error"       // an internal (store) failure — GitHub should retry
)

// DispatchResult summarizes what Dispatch did with one webhook event. The
// handler serializes it as the HTTP response body so the outcome is visible in
// GitHub's delivery record instead of being hidden behind a blind 200.
type DispatchResult struct {
	Event       string `json:"event"`
	Action      string `json:"action,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail,omitempty"`
	Scopes      int    `json:"scopes"`
}

// StatusCode maps a disposition to the HTTP status returned to GitHub:
//
//   - applied                       -> 200 OK   (data written to the cache)
//   - skipped / ignored / invalidated -> 202    (received, no data applied)
//   - error                         -> 500      (transient; GitHub retries)
//
// Every non-error outcome is 2xx so GitHub never disables a healthy webhook over
// a legitimate no-op, while the 200-vs-202 split lets an operator tell at a
// glance — in the deliveries list — whether a delivery actually updated the cache.
func (r DispatchResult) StatusCode() int {
	switch r.Disposition {
	case DispApplied:
		return http.StatusOK
	case DispError:
		return http.StatusInternalServerError
	default:
		return http.StatusAccepted
	}
}
