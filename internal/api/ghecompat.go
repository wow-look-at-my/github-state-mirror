package api

import (
	"net/http"
	"strings"
)

// gh treats any host that is not github.com as GitHub Enterprise Server: REST
// goes to /api/v3/..., GraphQL to /api/graphql. This file canonicalises those
// spellings BEFORE the router, so the cached routes, the reveal layer and the
// proxy keep seeing api.github.com paths. Without it every gh call misses the
// cache and reaches GitHub with the prefix still attached, where it is a miss.

const (
	gheRESTPrefix  = "/api/v3"
	gheGraphQLPath = "/api/graphql"
)

// gheCompat rewrites an enterprise-shaped path. Anything else passes untouched.
func gheCompat(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canon, ok := gheCanonicalPath(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// Copy: the request log and the timeline read the caller's request too.
		r2, u := *r, *r.URL
		if u.RawPath != "" {
			// RawPath is the escaped form. The prefix is ASCII, so it appears
			// there unchanged; if not, net/url derives it from Path again.
			u.RawPath, _ = gheCanonicalPath(u.RawPath)
		}
		u.Path = canon
		r2.URL = &u
		next.ServeHTTP(w, &r2)
	})
}

// gheCanonicalPath maps an enterprise path to its api.github.com spelling,
// reporting false when the path is not enterprise-shaped.
func gheCanonicalPath(p string) (string, bool) {
	if p == gheGraphQLPath {
		return "/graphql", true
	}
	rest, found := strings.CutPrefix(p, gheRESTPrefix)
	switch {
	case !found:
		return "", false
	case rest == "", rest == "/":
		// gh probes the REST root to check a host. Answer as the API root.
		return "/", true
	case strings.HasPrefix(rest, "/"):
		return rest, true
	}
	return "", false // /api/v3x keeps its own meaning.
}
