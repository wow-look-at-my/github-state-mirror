package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The repository archive redirects, .../tarball/{ref} and .../zipball/{ref}.

const (
	// archiveUnsignedTTL bounds a location with no credential and no expiry.
	archiveUnsignedTTL = 5 * time.Minute

	// archiveTokenLife is the documented life of a codeload token link.
	archiveTokenLife = 5 * time.Minute
	// archiveTokenMargin keeps a served link usable for the client's follow.
	archiveTokenMargin = time.Minute

	// archiveMaxRefLen bounds the ref key component.
	archiveMaxRefLen = 255
)

// cachedArchive serves GET /repos/{owner}/{repo}/{tarball,zipball}[/{ref}].
// The ref may carry slashes, so the route is a wildcard.
func (h *handlers) cachedArchive(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
		repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
		ref, ok := wildcardParam(r)
		if !ok || len(ref) > archiveMaxRefLen || strings.ContainsFunc(ref, isControlRune) {
			h.passthrough(w, r, PassPath)
			return
		}
		if !acceptsDefaultJSON(r) {
			h.passthrough(w, r, PassAccept)
			return
		}
		if len(r.URL.Query()) > 0 {
			h.passthrough(w, r, PassQuery)
			return
		}
		h.serveRepoAnswer(w, r, repoAnswerRoute{
			key:        ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: kind, RefID: ref},
			denyKind:   denyKindArchive,
			resource:   owner + "/" + repo + "/" + kind + "/" + ref,
			absorb:     absorbArchive,
			refusals:   []int{http.StatusNotFound},
			noRedirect: true,
		})
	}
}

// absorbArchive stores the archive redirect until its readable expiry. A
// expiry lives until it. One with no query at all carries no credential and
// lives archiveUnsignedTTL. A credential with no stated expiry is relayed.
func absorbArchive(resp *http.Response, _ []byte, now time.Time) (ghdata.RepoAnswer, bool) {
	if resp.StatusCode != http.StatusFound {
		return ghdata.RepoAnswer{}, false
	}
	loc := resp.Header.Get("Location")
	if exp, ok := signedURLExpiry(loc); ok {
		return redirectAnswer(loc, exp.Add(-redirectExpiryMargin))
	}
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() {
		return ghdata.RepoAnswer{}, false
	}
	if u.RawQuery == "" {
		return redirectAnswer(loc, now.Add(archiveUnsignedTTL))
	}
	// GitHub documents that a private repo's archive link expires after a few minutes.
	if strings.EqualFold(u.Host, "codeload.github.com") && u.Query().Get("token") != "" {
		return redirectAnswer(loc, now.Add(archiveTokenLife-archiveTokenMargin))
	}
	return ghdata.RepoAnswer{}, false
}
