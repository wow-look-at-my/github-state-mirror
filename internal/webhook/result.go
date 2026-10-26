// Disposition values describe what the dispatcher did with a delivery. log shown on the dashboard.

package webhook

import "net/http"

// Disposition values describe what the dispatcher did with a delivery. log
// shown on the dashboard.
const (
	DispApplied     = "applied"     // webhook data was written to global truth
	DispInvalidated = "invalidated" // marked freshness stale (fallback / structural change)
	DispIgnored     = "ignored"     // an event or action the mirror does not track
	DispError       = "error"       // an internal (store) failure — GitHub should retry
	// DispSuperseded means a strictly newer view of the same subject already applied; a success, not a failure.
	DispSuperseded = "superseded"
	// DispQueued is the answer to a delivery the journal now holds; the worker applies it.
	DispQueued = "queued"
	// DispDuplicate is the answer to a delivery whose id the journal already holds.
	DispDuplicate = "duplicate"
)

// DispatchResult summarizes what Dispatch did with webhook event. The
// handler serializes it as the HTTP response body so the outcome is visible in
// GitHub's delivery record instead of being hidden behind a blind.
type DispatchResult struct {
	Event       string `json:"event"`
	Action      string `json:"action,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail,omitempty"`
}

// StatusCode maps a disposition to GitHub's status: only error is non-2xx, applied is, the rest.
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
