// RulesetPayload is what a repository_ruleset delivery says: which ruleset changed, and where it lives.

package webhook

import (
	"encoding/json"
	"fmt"
)

// RulesetPayload is what a repository_ruleset delivery says: which ruleset
// changed, and where it lives.
type RulesetPayload struct {
	Owner     string
	Repo      string
	RulesetID int64
}

// ParseRulesetPayload reads a repository_ruleset delivery. A payload that
// names neither a repository nor an organization is an error.
func ParseRulesetPayload(raw json.RawMessage) (RulesetPayload, error) {
	var body struct {
		RepositoryRuleset struct {
			ID int64 `json:"id"`
		} `json:"repository_ruleset"`
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		Organization *struct {
			Login string `json:"login"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return RulesetPayload{}, fmt.Errorf("parse repository_ruleset payload: %w", err)
	}
	out := RulesetPayload{RulesetID: body.RepositoryRuleset.ID}
	switch {
	case body.Repository != nil && body.Repository.Owner.Login != "" && body.Repository.Name != "":
		out.Owner, out.Repo = body.Repository.Owner.Login, body.Repository.Name
	case body.Organization != nil && body.Organization.Login != "":
		out.Owner = body.Organization.Login
	default:
		return RulesetPayload{}, fmt.Errorf("parse repository_ruleset payload: names no repository or organization")
	}
	return out, nil
}
