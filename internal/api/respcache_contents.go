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
// `ref` query value matters. Both 200 (file or directory) and 404 are
// absorbed; the default JSON and raw file-body Accept shapes share one row.
// see docs/cache/rest-routes.md
func (h *handlers) cachedContents(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	path := chi.URLParam(r, "*")

	// Only the default JSON and raw file-body representations are absorbed; any other Accept (html/object, or a mix) passes through.
	rawAccept := acceptsRawContents(r)
	if !acceptsDefaultJSON(r) && !rawAccept {
		h.passthrough(w, r, PassAccept)
		return
	}
	// The contents endpoint takes exactly one query param, ref; anything else passes through.
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

	// Miss: fetch with the caller's credentials; a raw-Accept request still probes with the DEFAULT JSON Accept, other headers unchanged.
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
	// A 2xx is fresh proof of access -- renew the grant; a 404 is not proof either way, since the reveal layer already vouched for this read.
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

// serveContentsHit renders whichever Accept shape the caller asked for; a
// stored file row always has base64 content, decodable for a raw-Accept
// caller too, so there is no partial-row case to fall back from.
func (h *handlers) serveContentsHit(w http.ResponseWriter, r *http.Request, c ghdata.CachedContents, rawAccept bool) {
	if rawAccept && c.Kind == ghdata.ContentsKindFile {
		h.serveContentsRaw(w, r, c, true)
		return
	}
	h.serveContents(w, r, c, true)
}

// serveContentsRaw decodes a cached FILE row's base64 content and serves the
// exact file bytes, not the JSON wrapper.
// see docs/cache/rest-routes.md
func (h *handlers) serveContentsRaw(w http.ResponseWriter, r *http.Request, c ghdata.CachedContents, hit bool) {
	// c.Content includes GitHub's embedded line breaks, which base64 does not accept, so they are stripped before decoding.
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

// acceptsRawContents reports whether the request asks for GitHub's raw file-body media type.
// see docs/cache/rest-routes.md
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
