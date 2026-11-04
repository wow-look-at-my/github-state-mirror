// runCancelRefusal is the body of every refused cancel, shaped like a GitHub error.

package api

import (
	"net/http"
	"path"
	"strings"
)

// runCancelRefusal is the body of every refused cancel, shaped like a GitHub error.
type runCancelRefusal struct {
	Message string `json:"message"`
	Status  string `json:"status"`
}

const runCancelMessage = "github-state-mirror does not cancel workflow runs: cancel and force-cancel are refused for every caller"

// It runs after gheCompat, so the /api/v3 spelling is already canonical here.
func refuseRunCancel(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isRunCancelPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSONStatus(w, http.StatusForbidden, runCancelRefusal{Message: runCancelMessage, Status: "403"})
	})
}

// isRunCancelPath reports whether p names /repos/{owner}/{repo}/actions/runs/{run_id}/cancel
// or its force-cancel sibling. p is the decoded path, so an escaped spelling such as
// %63ancel matches. Dot segments, repeated or trailing slashes and letter case are
// normalised away, so no spelling GitHub would resolve to a cancel reaches it.
func isRunCancelPath(p string) bool {
	segs := strings.Split(strings.Trim(path.Clean("/"+p), "/"), "/")
	if len(segs) != 7 {
		return false
	}
	if !strings.EqualFold(segs[0], "repos") || !strings.EqualFold(segs[3], "actions") || !strings.EqualFold(segs[4], "runs") {
		return false
	}
	return strings.EqualFold(segs[6], "cancel") || strings.EqualFold(segs[6], "force-cancel")
}
