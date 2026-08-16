package webhook

import (
	"encoding/json"
	"strings"
)

// StatusEvent is a `status` delivery read as what it is: one commit status,
// whole. ParseCheckPayload reads the same body for the TRUTH side and
// normalizes as it goes (it prefixes the context with "status:" and folds the
// state into the mirror's own vocabulary), which is right for commit_checks
// and useless for rebuilding a response document -- that needs GitHub's own
// spelling of every field the document holds.
type StatusEvent struct {
	SHA         string
	Context     string
	State       string
	Description *string // nullable; the key is always emitted
	TargetURL   *string // nullable; the key is always emitted
	CreatedAt   string
	UpdatedAt   string
}

// ParseStatusEvent reads a `status` delivery's own account of the status it
// announces, or reports false when the payload states no usable one. A status
// is append-only upstream: re-posting a context creates a NEW status object
// with a new id and a new created_at, which is why created_at is required here
// -- it is what places the entry in a stored document's ordering.
func ParseStatusEvent(raw json.RawMessage) (StatusEvent, bool) {
	var body struct {
		SHA         string  `json:"sha"`
		Context     string  `json:"context"`
		State       string  `json:"state"`
		Description *string `json:"description"`
		TargetURL   *string `json:"target_url"`
		CreatedAt   string  `json:"created_at"`
		UpdatedAt   string  `json:"updated_at"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return StatusEvent{}, false
	}
	sha := strings.ToLower(body.SHA)
	if sha == "" || body.Context == "" || body.State == "" || body.CreatedAt == "" || body.UpdatedAt == "" {
		return StatusEvent{}, false
	}
	return StatusEvent{
		SHA: sha, Context: body.Context, State: body.State,
		Description: body.Description, TargetURL: body.TargetURL,
		CreatedAt: body.CreatedAt, UpdatedAt: body.UpdatedAt,
	}, true
}
