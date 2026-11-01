// The cached tags listing, GET /repos/{owner}/{repo}/tags.

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached tags listing, GET /repos/{owner}/{repo}/tags.

const (
	// GitHub's default per_page for the tags listing.
	tagsDefaultPerPage = 30

	// Caps modeled pages; deeper pagination passes through.
	tagsMaxCachedPage = 10
)

func (h *handlers) cachedTags(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	shape, ok := parsePagingShape(r.URL.Query(), tagsDefaultPerPage, tagsMaxCachedPage)
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	h.serveRepoAnswer(w, r, repoAnswerRoute{
		key:      ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindTags, Shape: shape},
		denyKind: denyKindTags,
		resource: owner + "/" + repo + "/tags?" + shape,
		absorb:   absorbTags,
		refusals: []int{http.StatusNotFound},
	})
}

func parsePagingShape(q url.Values, defaultPerPage, maxPage int) (string, bool) {
	perPage, page := defaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return "", false
		}
		n, err := strconv.Atoi(vals[0])
		switch key {
		case "per_page":
			if err != nil || n < 1 || n > 100 {
				return "", false
			}
			perPage = n
		case "page":
			if err != nil || n < 1 || n > maxPage {
				return "", false
			}
			page = n
		default:
			return "", false
		}
	}
	return "per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page), true
}

// Every entry needs a name and a full hex commit sha; an empty page is a
// valid answer.
func absorbTags(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
	if resp.StatusCode != http.StatusOK {
		return ghdata.RepoAnswer{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ghdata.RepoAnswer{}, false
	}
	var raw []ghdata.StoredTag
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return ghdata.RepoAnswer{}, false
	}
	for _, tag := range raw {
		if tag.Name == "" || !ghdata.IsFullHexSHA(tag.Commit.SHA) {
			return ghdata.RepoAnswer{}, false
		}
	}
	if raw == nil {
		raw = []ghdata.StoredTag{}
	}
	doc, err := marshalTrimmed(raw)
	if err != nil {
		return ghdata.RepoAnswer{}, false
	}
	return ghdata.RepoAnswer{Status: http.StatusOK, Doc: string(doc), ExpiresAt: now.Add(ghdata.TagsCacheTTL)}, true
}
