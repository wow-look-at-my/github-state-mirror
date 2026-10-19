package api

import "net/http"

// notGitHubPaths are route patterns that GitHub's API does not have.
var notGitHubPaths = []string{"/wp-json/*"}

// notGitHub gives a scanner probe the Not Found answer that GitHub gives.
func (h *handlers) notGitHub(w http.ResponseWriter, r *http.Request) {
	body, err := marshalTrimmed(notFoundJSON{Message: "Not Found", Status: "404"})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.reqlog.observeStatus(r, DispHit, http.StatusNotFound)
	writeRebuilt(w, http.StatusNotFound, body, true)
}
