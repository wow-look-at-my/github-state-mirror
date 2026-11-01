// IssuePayload is an issues or issue_comment delivery: the repo, the action, and the whole issue object, verbatim.

package webhook

import (
	"encoding/json"
	"fmt"
)

// IssuePayload is an issues or issue_comment delivery: the repo, the action,
// and the whole issue object, verbatim.
type IssuePayload struct {
	Owner  string
	Repo   string
	Action string
	Number int64
	Issue  json.RawMessage
}

// ParseIssuePayload extracts the issue object an issues or issue_comment
// delivery carries.
func ParseIssuePayload(raw json.RawMessage) (IssuePayload, error) {
	var body struct {
		Action     string          `json:"action"`
		Issue      json.RawMessage `json:"issue"`
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return IssuePayload{}, fmt.Errorf("parse issue payload: %w", err)
	}
	if body.Repository == nil || body.Repository.Owner.Login == "" || body.Repository.Name == "" {
		return IssuePayload{}, fmt.Errorf("parse issue payload: missing owner/repo")
	}
	var issue struct {
		Number int64 `json:"number"`
	}
	if len(body.Issue) == 0 || json.Unmarshal(body.Issue, &issue) != nil || issue.Number <= 0 {
		return IssuePayload{}, fmt.Errorf("parse issue payload: missing issue number")
	}
	return IssuePayload{
		Owner: body.Repository.Owner.Login, Repo: body.Repository.Name,
		Action: body.Action, Number: issue.Number, Issue: body.Issue,
	}, nil
}
