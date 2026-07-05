package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// Streaming mode for the admin consistency check / reconcile:
//
//	GET  /api/cache/check?stream=1[&org=<o>]
//	POST /api/cache/check?stream=1&apply=true[&org=<o>]
//
// A real fleet run takes minutes (per owner: the paginated GetOwnerData fetch
// at 5 repos/page, the visibility fetch, the diff, and in apply mode the
// corrections), so with ?stream=1 the endpoint answers application/x-ndjson
// and writes one JSON line per checker progress event, flushing after every
// line so the reverse proxy in front (Cloudflare) relays them live instead of
// buffering the whole run. The FINAL line is
//
//	{"phase":"report","report":<ConsistencyReport>}
//
// carrying the exact report the non-stream path returns -- or, when the run
// fails after the 200 has already been committed,
//
//	{"phase":"error","error":"..."}
//
// Non-stream requests are untouched: one buffered application/json report.

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
	// The line stream must reach the operator live: never cached, and
	// buffering proxies (nginx honors X-Accel-Buffering) told not to hold it.
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
	// The run deliberately stays on r.Context(): the operator closing the
	// modal (aborting the fetch) cancels it mid-flight. That is safe -- a
	// read-only check writes nothing, and every apply correction is
	// idempotent, so a re-run simply redoes the remainder.
	report, err := run(r.Context(), org, func(ev syncpkg.ProgressEvent) {
		writeLine(checkStreamLine{ProgressEvent: ev})
	})
	if err != nil {
		// The 200 + partial body is already committed; the error line IS the
		// error channel (mirrors the non-stream 502 body text).
		slog.Warn("consistency check failed", "apply", apply, "stream", true, "error", err)
		writeLine(checkStreamLine{ProgressEvent: syncpkg.ProgressEvent{Phase: "error"}, Error: "consistency check failed: " + err.Error()})
		return true
	}
	writeLine(checkStreamLine{ProgressEvent: syncpkg.ProgressEvent{Phase: "report"}, Report: report})
	return true
}
