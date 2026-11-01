// The rules that apply to a branch, GET /repos/{owner}/{repo}/rules/branches/{branch}.

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The rules that apply to a branch, GET /repos/{owner}/{repo}/rules/branches/{branch}.

const (
	branchRulesDefaultPerPage = 30
	branchRulesMaxCachedPage  = 10
	branchRulesMaxBranchLen   = 255
)

func (h *handlers) cachedBranchRules(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	branch, ok := wildcardParam(r)
	if !ok || branch == "" || len(branch) > branchRulesMaxBranchLen || strings.ContainsFunc(branch, isControlRune) {
		h.passthrough(w, r, PassPath)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	shape, ok := parsePagingShape(r.URL.Query(), branchRulesDefaultPerPage, branchRulesMaxCachedPage)
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	h.serveRepoAnswer(w, r, repoAnswerRoute{
		key:      ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindBranchRules, RefID: branch, Shape: shape},
		denyKind: denyKindBranchRules,
		resource: owner + "/" + repo + "/rules/branches/" + branch + "?" + shape,
		absorb:   absorbBranchRules,
		refusals: []int{http.StatusNotFound},
	})
}

// branchRuleJSON is one rule in force on the branch. parameters is the
// rule type's own object, kept whole and present only when GitHub sends it.
type branchRuleJSON struct {
	Type              string          `json:"type"`
	RulesetSourceType string          `json:"ruleset_source_type"`
	RulesetSource     string          `json:"ruleset_source"`
	RulesetID         int64           `json:"ruleset_id"`
	Parameters        json.RawMessage `json:"parameters,omitempty"`
}

func absorbBranchRules(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
	if resp.StatusCode != http.StatusOK {
		return ghdata.RepoAnswer{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ghdata.RepoAnswer{}, false
	}
	var items []branchRuleJSON
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return ghdata.RepoAnswer{}, false
	}
	for _, item := range items {
		if item.Type == "" || item.RulesetID <= 0 {
			return ghdata.RepoAnswer{}, false
		}
	}
	if items == nil {
		items = []branchRuleJSON{}
	}
	doc, err := marshalTrimmed(items)
	if err != nil {
		return ghdata.RepoAnswer{}, false
	}
	return ghdata.RepoAnswer{Status: http.StatusOK, Doc: string(doc), ExpiresAt: now.Add(ghdata.BranchRulesTTL)}, true
}
