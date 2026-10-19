package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached GraphQL PR-list read: repository(owner, name) { pullRequests(...) }. The shape is what `gh pr list` sends.

const graphqlPullsTTL = 10 * time.Minute

// graphqlPullsArgs are the pullRequests arguments the key models. A
// pull_request delivery moves the answer for each of them.
var graphqlPullsArgs = map[string]bool{
	"states": true, "baseRefName": true, "headRefName": true,
	"first": true, "last": true, "after": true, "before": true, "orderBy": true,
}

// graphqlPullFields are the PR fields only a pull_request delivery changes.
var graphqlPullFields = map[string]bool{
	"id": true, "number": true, "title": true, "state": true, "isDraft": true,
	"headRefName": true, "baseRefName": true, "headRefOid": true,
	"createdAt": true, "updatedAt": true, "closedAt": true, "mergedAt": true,
	"isCrossRepository": true, "__typename": true,
}

var graphqlPageInfoFields = map[string]bool{
	"hasNextPage": true, "hasPreviousPage": true, "endCursor": true, "startCursor": true, "__typename": true,
}

// graphqlPullsQuery is a matched request.
type graphqlPullsQuery struct {
	owner, repo string
	shape       string
}

// matchGraphQLPulls reports whether body is the cached PR-list shape. The
// shape key holds the canonical token stream, the operation name and the
// variables, so equal keys ask GitHub the same question.
func matchGraphQLPulls(body []byte) (graphqlPullsQuery, bool) {
	var req struct {
		Query         string          `json:"query"`
		OperationName string          `json:"operationName"`
		Variables     json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return graphqlPullsQuery{}, false
	}
	vars := map[string]any{}
	if len(bytes.TrimSpace(req.Variables)) > 0 && !bytes.Equal(bytes.TrimSpace(req.Variables), []byte("null")) {
		dec := json.NewDecoder(bytes.NewReader(req.Variables))
		dec.UseNumber()
		if err := dec.Decode(&vars); err != nil {
			return graphqlPullsQuery{}, false
		}
	}
	doc, err := parseGraphQL(req.Query)
	if err != nil || len(doc.Operations) != 1 {
		return graphqlPullsQuery{}, false
	}
	op := doc.Operations[0]
	if op.Kind != "query" || (req.OperationName != "" && req.OperationName != op.Name) {
		return graphqlPullsQuery{}, false
	}
	if len(op.Selection) != 1 || op.Selection[0].Field != "repository" || op.Selection[0].Alias != "" {
		return graphqlPullsQuery{}, false
	}
	repoSel := op.Selection[0]
	if len(repoSel.Args) != 2 {
		return graphqlPullsQuery{}, false
	}
	owner, ok1 := graphqlStringArg(repoSel.Args["owner"], vars)
	repo, ok2 := graphqlStringArg(repoSel.Args["name"], vars)
	if !ok1 || !ok2 || owner == "" || repo == "" {
		return graphqlPullsQuery{}, false
	}
	if len(repoSel.Selection) != 1 || !graphqlPullsConnection(repoSel.Selection[0], doc) {
		return graphqlPullsQuery{}, false
	}
	canonVars, err := json.Marshal(vars)
	if err != nil {
		return graphqlPullsQuery{}, false
	}
	sum := sha256.Sum256([]byte(doc.Canonical + "\x00" + op.Name + "\x00" + string(canonVars)))
	return graphqlPullsQuery{owner: owner, repo: repo, shape: hex.EncodeToString(sum[:])}, true
}

// graphqlStringArg resolves a string literal or a string variable.
func graphqlStringArg(v gqlValue, vars map[string]any) (string, bool) {
	if v.Var != "" {
		s, ok := vars[v.Var].(string)
		return s, ok
	}
	return v.Str, v.IsStr
}

func graphqlPullsConnection(sel gqlSelection, doc gqlDocument) bool {
	if sel.Field != "pullRequests" || sel.Alias != "" || len(sel.Selection) == 0 {
		return false
	}
	for arg := range sel.Args {
		if !graphqlPullsArgs[arg] {
			return false
		}
	}
	for _, s := range sel.Selection {
		if s.Alias != "" || s.Spread != "" || len(s.Args) > 0 {
			return false
		}
		switch s.Field {
		case "totalCount":
			if s.Selection != nil {
				return false
			}
		case "pageInfo":
			if !graphqlScalars(s.Selection, graphqlPageInfoFields, nil) {
				return false
			}
		case "nodes":
			if !graphqlScalars(s.Selection, graphqlPullFields, &doc) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// graphqlScalars reports that every selection is an allowed scalar field.
// With doc set, a spread of a PullRequest fragment of allowed scalars counts.
func graphqlScalars(sels []gqlSelection, allowed map[string]bool, doc *gqlDocument) bool {
	if len(sels) == 0 {
		return false
	}
	for _, s := range sels {
		if s.Spread != "" {
			if doc == nil {
				return false
			}
			frag, ok := doc.Fragments[s.Spread]
			if !ok || frag.On != "PullRequest" || !graphqlScalars(frag.Selection, allowed, nil) {
				return false
			}
			continue
		}
		if s.Alias != "" || len(s.Args) > 0 || s.Selection != nil || !allowed[s.Field] {
			return false
		}
	}
	return true
}

// serveGraphQLPulls answers a matched PR-list query. A caller the reveal layer
// does not clear is forwarded, so GitHub states its own GraphQL error.
func (h *handlers) serveGraphQLPulls(w http.ResponseWriter, r *http.Request, body []byte, q graphqlPullsQuery) {
	forward := func() {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		ctx := withDispositionHint(r.Context(), DispPassthrough)
		h.ghProxy.ServeHTTP(w, r.WithContext(withPassthroughReason(ctx, PassGraphQL)))
	}
	owner, repo := ghdata.NormalizeRepoKey(q.owner), ghdata.NormalizeRepoKey(q.repo)
	if outcome, _, _ := h.reveal(r, owner, repo, denyKindRepoPulls, owner+"/"+repo+"/pulls"); outcome != revealAllowed {
		forward()
		return
	}

	now := time.Now()
	key := ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindGraphQLPulls, Shape: q.shape}
	if a, ok, err := h.store.GetRepoAnswer(r.Context(), key, now); err != nil {
		slog.Warn("graphql pulls cache read failed", "owner", owner, "repo", repo, "error", err)
	} else if ok {
		h.reqlog.observe(r, DispHit)
		writeRebuilt(w, http.StatusOK, []byte(a.Doc), true)
		return
	}

	resp, respBody, overflow, err := h.fetchUpstream(r, body)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()
	if overflow || resp.StatusCode != http.StatusOK || !graphqlPullsAnswer(respBody) {
		h.replayUnstored(w, r, resp, respBody)
		return
	}
	a := ghdata.RepoAnswer{Status: http.StatusOK, Doc: string(respBody), ExpiresAt: now.Add(graphqlPullsTTL)}
	if err := h.store.PutRepoAnswer(r.Context(), key, a, now); err != nil {
		slog.Warn("graphql pulls cache write failed", "owner", owner, "repo", repo, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, respBody, false)
}

// graphqlPullsAnswer accepts only a clean answer: no errors key at all, and
// a non-null connection.
func graphqlPullsAnswer(body []byte) bool {
	var doc struct {
		Data *struct {
			Repository *struct {
				PullRequests *json.RawMessage `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
		Errors *json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Errors != nil {
		return false
	}
	return doc.Data != nil && doc.Data.Repository != nil && doc.Data.Repository.PullRequests != nil &&
		!bytes.Equal(bytes.TrimSpace(*doc.Data.Repository.PullRequests), []byte("null"))
}
