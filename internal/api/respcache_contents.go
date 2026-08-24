package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file is the whole GET /repos/{owner}/{repo}/contents/{path...} route
// family: the absorb/render machinery and BOTH Accept shapes the route models.
// respcache.go keeps the machinery every cached route shares.

// cachedContents serves repo contents from absorbed state, fetching and
// absorbing on a miss. Cache key: (actor, owner, repo, path, ref) — the raw
// `ref` query value matters (`contents?ref=...` differs per ref). Both 200
// (file or directory) and 404 ("config file absent" — half the win for
// pr-minder's per-repo config probe) are absorbed.
//
// TWO Accept shapes are modeled from the SAME absorbed row: the default JSON
// representation (base64 `content` field) and the raw file-body
// representation (Accept: application/vnd.github.raw, what simple-llm-ui's
// file-read tool sends — docs/tools/filesystem-tools.md in that repo). They
// are the same underlying bytes, so there is no second cache dimension: a
// raw-Accept request is served by base64-DECODING the already-absorbed
// `content` (serveContentsRaw) rather than caching a second copy, and a miss
// is always fetched with the DEFAULT JSON Accept regardless of which shape
// the caller asked for — mirroring go-github, PyGithub and octokit.js, which
// all resolve file-vs-directory via the default JSON shape first and never
// send the raw media type against a path that might be a directory (GitHub's
// behavior there is undocumented). The one place this can't be made to work
// -- a file too large for the base64 JSON form (>1 MiB; GitHub answers
// encoding:"none" past that, only raw/object media types carry the bytes,
// per docs.github.com/en/rest/repos/contents) -- falls back to a second,
// uncached fetchUpstream call with the caller's own raw Accept header,
// replayed verbatim (replayUnstored) rather than routed through the
// passthrough proxy's debouncer, which exists to price shapes this mirror
// could still learn to model, not ones that are structurally unabsorbable.
func (h *handlers) cachedContents(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	path := chi.URLParam(r, "*")

	// Only the default JSON representation and the raw file-body
	// representation are absorbed (see the file-header comment above). Any
	// other Accept media type (html/object, or an ambiguous mix) changes the
	// response shape entirely — passthrough.
	rawAccept := acceptsRawContents(r)
	if !acceptsDefaultJSON(r) && !rawAccept {
		h.passthrough(w, r, PassAccept)
		return
	}
	// The contents endpoint takes exactly one query param, ref. Anything else
	// is a shape we don't model — passthrough.
	q := r.URL.Query()
	ref := q.Get("ref")
	delete(q, "ref")
	if len(q) > 0 {
		h.passthrough(w, r, PassQuery)
		return
	}

	// Reveal: may this caller read the repo's cached state?
	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindContents, contentsResourceKey(owner, repo, path, ref)); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if c, ok, err := h.store.GetCachedContents(r.Context(), owner, repo, path, ref, now); err == nil && ok {
		h.serveContentsHit(w, r, c, rawAccept)
		return
	} else if err != nil {
		slog.Warn("contents cache read failed", "owner", owner, "repo", repo, "path", path, "error", err)
	}

	// Miss: fetch from GitHub with the caller's own credentials. A raw-Accept
	// request is still probed with the DEFAULT JSON Accept (see the file
	// header) — only the caller's Authorization and other headers ride along
	// unchanged.
	probeReq := r
	if rawAccept {
		probeReq = r.Clone(r.Context())
		probeReq.Header = r.Header.Clone()
		probeReq.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, body, overflow, err := h.fetchUpstream(probeReq, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	c, absorbed := absorbContents(owner, repo, path, ref, resp.StatusCode, body)
	if overflow || !absorbed {
		if rawAccept {
			// The JSON probe couldn't carry this response — most likely a
			// file over the ~1 MiB base64 threshold, which GitHub answers
			// with encoding:"none" and no content past that size (only
			// raw/object media types work there). Fetch again with the
			// caller's OWN raw Accept header (r, unmodified) and replay it
			// verbatim via fetchUpstream/replayUnstored directly — NOT
			// h.passthrough, which would route this second call through the
			// passthrough proxy's debouncer: that hold exists to price
			// shapes this mirror could still learn to model, not ones that
			// are structurally unabsorbable regardless of Accept.
			rawResp, rawBody, _, rawErr := h.fetchUpstream(r, nil)
			if rawErr != nil {
				h.upstreamError(w, r, rawErr)
				return
			}
			defer rawResp.Body.Close()
			h.replayUnstored(w, r, rawResp, rawBody)
			return
		}
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedContents(r.Context(), c, now, contentsCacheTTL); err != nil {
		slog.Warn("contents cache write failed", "owner", owner, "repo", repo, "path", path, "error", err)
	}
	// A 2xx with the caller's own token is fresh proof of access -- renew the
	// grant so steady consumers never age out mid-use. (A 404 is not proof
	// either way; the reveal layer already vouched for this read.)
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	if rawAccept && c.Kind == ghdata.ContentsKindFile {
		h.serveContentsRaw(w, r, c, false)
		return
	}
	h.serveContents(w, r, c, false)
}

// serveContents rebuilds and writes the trimmed contents response.
func (h *handlers) serveContents(w http.ResponseWriter, r *http.Request, c ghdata.CachedContents, hit bool) {
	status, body, err := renderContents(c)
	if err != nil {
		slog.Warn("contents cache render failed", "path", c.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hit {
		h.reqlog.observe(r, DispHit)
	}
	writeRebuilt(w, status, body, hit)
}

// contentsFileJSON is the trimmed rebuild of a file response: GitHub's shape
// minus url/git_url/html_url/download_url/_links.
type contentsFileJSON struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int64  `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	SHA      string `json:"sha"`
}

// contentsEntryJSON is one trimmed directory-listing entry.
type contentsEntryJSON struct {
	Type string `json:"type"`
	Size int64  `json:"size"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

// notFoundJSON is the trimmed rebuild of a 404: GitHub's message + status,
// documentation_url dropped.
type notFoundJSON struct {
	Message string `json:"message"`
	Status  string `json:"status"`
}

// absorbContents parses an upstream contents response into cacheable state.
// It absorbs a 200 file (base64-encoded — the >1 MiB "encoding":"none" form
// is not modeled), a 200 directory listing, and a 404. Anything else — other
// statuses, symlink/submodule objects, unexpected shapes — reports false and
// is served verbatim, unstored.
func absorbContents(owner, repo, path, ref string, status int, body []byte) (ghdata.CachedContents, bool) {
	c := ghdata.CachedContents{Owner: owner, Repo: repo, Path: path, Ref: ref}
	switch status {
	case http.StatusOK:
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 {
			return c, false
		}
		if trimmed[0] == '[' { // directory listing
			var raw []struct {
				Type string `json:"type"`
				Size int64  `json:"size"`
				Name string `json:"name"`
				Path string `json:"path"`
				SHA  string `json:"sha"`
			}
			if err := json.Unmarshal(trimmed, &raw); err != nil {
				return c, false
			}
			entries := make([]contentsEntryJSON, 0, len(raw))
			for _, e := range raw {
				entries = append(entries, contentsEntryJSON(e))
			}
			rendered, err := marshalTrimmed(entries)
			if err != nil {
				return c, false
			}
			c.Kind = ghdata.ContentsKindDir
			c.Entries = string(rendered)
			return c, true
		}
		var f struct {
			Type     string  `json:"type"`
			Encoding string  `json:"encoding"`
			Size     int64   `json:"size"`
			Name     string  `json:"name"`
			Path     string  `json:"path"`
			Content  *string `json:"content"`
			SHA      string  `json:"sha"`
		}
		if err := json.Unmarshal(trimmed, &f); err != nil {
			return c, false
		}
		if f.Type != "file" || f.Encoding != "base64" || f.Content == nil || f.SHA == "" {
			return c, false
		}
		c.Kind = ghdata.ContentsKindFile
		c.Name, c.SHA, c.Size, c.Encoding, c.Content = f.Name, f.SHA, f.Size, f.Encoding, *f.Content
		return c, true
	case http.StatusNotFound:
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = "Not Found"
		}
		c.Kind = ghdata.ContentsKindMissing
		c.Message = e.Message
		return c, true
	default:
		return c, false
	}
}

// renderContents rebuilds the trimmed response body for absorbed contents
// state. Hits and misses both go through here, so the served shape is
// identical regardless of cache state.
func renderContents(c ghdata.CachedContents) (int, []byte, error) {
	switch c.Kind {
	case ghdata.ContentsKindFile:
		body, err := marshalTrimmed(contentsFileJSON{
			Type: "file", Encoding: c.Encoding, Size: c.Size,
			Name: c.Name, Path: c.Path, Content: c.Content, SHA: c.SHA,
		})
		return http.StatusOK, body, err
	case ghdata.ContentsKindDir:
		return http.StatusOK, []byte(c.Entries), nil
	case ghdata.ContentsKindMissing:
		body, err := marshalTrimmed(notFoundJSON{Message: c.Message, Status: "404"})
		return http.StatusNotFound, body, err
	default:
		return 0, nil, fmt.Errorf("unknown contents kind %q", c.Kind)
	}
}

// serveContentsHit renders a cache HIT for whichever Accept shape the caller
// asked for. A stored row always has everything either shape needs: a `file`
// row is only ever created by absorbContents's JSON branch (it requires
// encoding:"base64"), so its `content` is always populated and decodable for
// a raw-Accept caller too — there is no partial-row case to fall back from.
func (h *handlers) serveContentsHit(w http.ResponseWriter, r *http.Request, c ghdata.CachedContents, rawAccept bool) {
	if rawAccept && c.Kind == ghdata.ContentsKindFile {
		h.serveContentsRaw(w, r, c, true)
		return
	}
	h.serveContents(w, r, c, true)
}

// serveContentsRaw decodes a cached FILE row's base64 `content` and serves it
// as GitHub's raw media type would: the exact file bytes, not the JSON
// wrapper. Reusing the SAME stored base64 the default-Accept representation
// renders from means one absorb feeds both Accept shapes (see the file
// header), and since base64 round-trips arbitrary bytes exactly, this is
// correct for binary content too, up to the size the base64 JSON form
// carries at all (past it, cachedContents falls back to a genuine
// passthrough instead of trying to serve from this cache).
//
// Content-Type is fixed at "text/plain; charset=utf-8" rather than replaying
// whatever a live GitHub raw response would carry (GitHub does not document
// this, and it is only weakly evidenced in the wild as approximately this
// value) — every known consumer of this shape (simple-llm-ui's file-read
// tool, agentic-loop's repo client) detects binary content by inspecting the
// BODY (UTF-8 validity / NUL bytes), never Content-Type, and MUST NOT see a
// "json"-containing Content-Type here: agentic-loop's IsDirectoryListing
// treats any body starting with '[' under a json Content-Type as a directory
// listing, which would misidentify a text file whose content happens to
// start with '[' (e.g. reading a JSON array file as raw text).
func (h *handlers) serveContentsRaw(w http.ResponseWriter, r *http.Request, c ghdata.CachedContents, hit bool) {
	// c.Content is base64 EXACTLY as GitHub sent it, including the embedded
	// line breaks GitHub's own MIME-style wrapping inserts (documented on
	// ghdata.CachedContents.Content) -- even a one-line "aGVsbG8=\n" carries
	// a trailing one. encoding/base64 does not accept those as part of the
	// alphabet, so they are stripped before decoding rather than assumed
	// away; every other consumer of this field only ever REPLAYS it as text
	// and never had to care.
	clean := strings.NewReplacer("\r", "", "\n", "").Replace(c.Content)
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		slog.Warn("contents raw decode failed", "owner", c.Owner, "repo", c.Repo, "path", c.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hit {
		h.reqlog.observe(r, DispHit)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if hit {
		w.Header().Set(cacheHeader, "hit")
	} else {
		w.Header().Set(cacheHeader, "miss")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// acceptsRawContents reports whether the request asks for GitHub's raw
// file-body media type — the shape simple-llm-ui's file-read tool sends
// (Accept: application/vnd.github.raw; the +json and legacy v3 spellings are
// accepted too, the acceptsDefaultJSON precedent). Every listed media range
// must be a raw one, same rule as acceptsDefaultJSON; an EMPTY Accept is
// deliberately NOT raw here (acceptsDefaultJSON already claims that case, and
// the two must remain mutually exclusive so cachedContents's top-level gate
// never double-counts a request).
func acceptsRawContents(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.TrimSpace(accept) == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.ToLower(part))
		if i := strings.IndexByte(mediaType, ';'); i >= 0 {
			mediaType = strings.TrimSpace(mediaType[:i])
		}
		switch mediaType {
		case "application/vnd.github.raw", "application/vnd.github.raw+json", "application/vnd.github.v3.raw":
			// raw file-body representation
		default:
			return false
		}
	}
	return true
}
