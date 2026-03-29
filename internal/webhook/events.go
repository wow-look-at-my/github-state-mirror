package webhook

import "encoding/json"

// Event is a parsed webhook event with just enough info for dispatch.
type Event struct {
	Type   string // X-GitHub-Event header value
	Action string // "action" field from payload

	// Repository info (extracted from payload.repository)
	RepoOwnerLogin string
	RepoNameStr    string

	// PR info (extracted from payload.pull_request if present)
	PRNumber int64
	PRBase   string
	PRHead   string

	// Org info
	OrgLogin string

	// Raw payload for anything that needs deeper inspection.
	Raw json.RawMessage
}

func (e Event) RepoOwner() string { return e.RepoOwnerLogin }
func (e Event) RepoName() string  { return e.RepoNameStr }
func (e Event) RepoFullName() string {
	if e.RepoOwnerLogin == "" || e.RepoNameStr == "" {
		return ""
	}
	return e.RepoOwnerLogin + "/" + e.RepoNameStr
}

// ParseEvent extracts an Event from a raw webhook payload and event type header.
func ParseEvent(eventType string, payload []byte) Event {
	e := Event{
		Type: eventType,
		Raw:  payload,
	}

	var body struct {
		Action string `json:"action"`
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		PullRequest *struct {
			Number int `json:"number"`
			Base   *struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Head *struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
		Organization *struct {
			Login string `json:"login"`
		} `json:"organization"`
	}

	if err := json.Unmarshal(payload, &body); err != nil {
		return e
	}

	e.Action = body.Action
	if body.Repository != nil {
		e.RepoOwnerLogin = body.Repository.Owner.Login
		e.RepoNameStr = body.Repository.Name
	}
	if body.PullRequest != nil {
		e.PRNumber = int64(body.PullRequest.Number)
		if body.PullRequest.Base != nil {
			e.PRBase = body.PullRequest.Base.Ref
		}
		if body.PullRequest.Head != nil {
			e.PRHead = body.PullRequest.Head.Ref
		}
	}
	if body.Organization != nil {
		e.OrgLogin = body.Organization.Login
	}

	return e
}
