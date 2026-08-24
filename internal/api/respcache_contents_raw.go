package api

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file implements the raw file-body Accept shape of cachedContents
// (respcache.go) -- Accept: application/vnd.github.raw, what simple-llm-ui's
// file-read tool sends. See cachedContents's own file header for the shared
// design: both this shape and the default JSON one are modeled from the SAME
// absorbed contents_cache row, so this file adds no cache dimension, no
// schema, and no fetch of its own beyond the fallback cachedContents already
// documents.

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
