package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file holds the contents route's render/absorb/serve helpers, split from
// respcache.go (over the 500-line warning threshold once raw-Accept support
// landed). Two natural groups live here:
//   - the default JSON representation (serveContents, absorbContents,
//     renderContents, and the trimmed JSON types), originally in respcache.go;
//   - the raw file-body Accept shape (serveContentsRaw, acceptsRawContents),
//     added alongside them since both render from the SAME absorbed row (see
//     cachedContents's file header for the shared design).

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

// ---- Default JSON representation (moved from respcache.go) ----

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
