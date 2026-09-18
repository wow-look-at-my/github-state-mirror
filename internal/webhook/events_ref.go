package webhook

import (
	"encoding/json"
	"fmt"
)

// A create or delete delivery states which ref appeared or vanished, and
// whether it is a branch or a tag. It carries NO sha, so a create cannot write
// a tip. What it can do is say that a cached answer about this ref is wrong.
type RefLifecyclePayload struct {
	Owner string
	Repo  string
	Ref   string // the SHORT name here, where a push sends the qualified form
	// "branch" or "tag": a tag and a branch sharing a name key differently.
	RefType  string
	PushedAt string // repository.pushed_at, the clock this delivery orders by
}

// IsTag reports whether the delivery is about a tag.
func (p RefLifecyclePayload) IsTag() bool { return p.RefType == "tag" }

// ParseRefLifecyclePayload reads a create or delete delivery.
func ParseRefLifecyclePayload(raw json.RawMessage) (RefLifecyclePayload, error) {
	var body struct {
		Ref        string `json:"ref"`
		RefType    string `json:"ref_type"`
		Repository *struct {
			Name     string `json:"name"`
			PushedAt any    `json:"pushed_at"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return RefLifecyclePayload{}, fmt.Errorf("parse ref lifecycle payload: %w", err)
	}
	if body.Repository == nil {
		return RefLifecyclePayload{}, fmt.Errorf("parse ref lifecycle payload: no repository field")
	}
	if body.Ref == "" {
		return RefLifecyclePayload{}, fmt.Errorf("parse ref lifecycle payload: no ref")
	}
	return RefLifecyclePayload{
		Owner:    body.Repository.Owner.Login,
		Repo:     body.Repository.Name,
		Ref:      body.Ref,
		RefType:  body.RefType,
		PushedAt: timestampString(body.Repository.PushedAt),
	}, nil
}
