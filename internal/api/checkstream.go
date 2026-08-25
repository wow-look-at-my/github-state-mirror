package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// Streaming mode for the admin consistency check / reconcile.
// see docs/dashboard/operator-tooling.md

// checkStreamLine is one NDJSON line: a checker progress event, extended with
// the terminal report/error fields for the final line.
type checkStreamLine struct {
	syncpkg.ProgressEvent
	Report *syncpkg.ConsistencyReport `json:"report,omitempty"`
	Error  string                     `json:"error,omitempty"`
}

// streamCacheCheck runs the consistency check (or reconcile, when apply) with
// live NDJSON progress. It returns false -- having written nothing -- when the
// ResponseWriter cannot stream (no http.Flusher), so the caller falls back to
// the buffered non-stream path. Admin/apply/availability gates have already
// run in handleCacheCheck.
func (d *dashboard) streamCacheCheck(w http.ResponseWriter, r *http.Request, org string, apply bool) bool {
	fl, ok := w.(http.Flusher)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	// Never cache; tell proxies not to buffer this.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	enc := json.NewEncoder(w)
	writeLine := func(line checkStreamLine) {
		// A failed write means the client is gone; the run is aborted by
		// r.Context() shortly, so just stop writing.
		if err := enc.Encode(line); err != nil {
			return
		}
		fl.Flush()
	}

	run := d.checker.CheckWithProgress
	if apply {
		run = d.checker.CheckAndApplyWithProgress
	}
	// Stays on r.Context(): a check writes nothing and apply is idempotent, so cancelling mid-run is safe.
	report, err := run(r.Context(), org, func(ev syncpkg.ProgressEvent) {
		writeLine(checkStreamLine{ProgressEvent: ev})
	})
	if err != nil {
		// The error line is the error channel; it mirrors the non-stream 502 body.
		slog.Warn("consistency check failed", "apply", apply, "stream", true, "error", err)
		writeLine(checkStreamLine{ProgressEvent: syncpkg.ProgressEvent{Phase: "error"}, Error: "consistency check failed: " + err.Error()})
		return true
	}
	writeLine(checkStreamLine{ProgressEvent: syncpkg.ProgressEvent{Phase: "report"}, Report: report})
	return true
}
