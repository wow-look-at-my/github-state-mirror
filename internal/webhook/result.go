package webhook

import "net/http"

// Disposition values describe what the dispatcher did with a delivery.
// log shown on the dashboard.
//
// There is deliberately no "skipped" anymore: since the global-cache
// re-architecture every stateful event applies straight to global truth (there
// is no per-scope gate to skip on). "ignored" remains only for event types and
// actions the mirror genuinely does not track.
const (
	DispApplied     = "applied"     // webhook data was written to global truth
	DispInvalidated = "invalidated" // marked freshness stale (fallback / structural change)
	DispIgnored     = "ignored"     // an event or action the mirror does not track
	DispError       = "error"       // an internal (store) failure — GitHub should retry
	// DispSuperseded means a strictly newer view of the same subject already applied; a success, not a failure.
	DispSuperseded = "superseded"
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
}

// StatusCode maps a disposition to GitHub's status: only error is non-2xx, applied is 200, the rest 202.
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
