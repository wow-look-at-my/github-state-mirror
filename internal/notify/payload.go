package notify

import (
	"encoding/json"

	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// Notification is the compact JSON body POSTed to a subscriber after the
// mirror ingested a GitHub delivery. MirrorDelivery is unique per
// subscription x delivery; GitHubDelivery is the X-GitHub-Delivery GUID the
// MIRROR received — it differs from the GUID GitHub sent the subscriber for
// the same event, so owner/repo plus the identifier fields (pr_number, ref,
// sha) are the correlation keys.
type Notification struct {
	MirrorDelivery string `json:"mirror_delivery"`
	SubscriptionID string `json:"subscription_id"`
	GitHubDelivery string `json:"github_delivery,omitempty"`
	Event          string `json:"event"`
	Action         string `json:"action,omitempty"`
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	RepoFullName   string `json:"repo_full_name"`
	PRNumber       int64  `json:"pr_number,omitempty"`
	Ref            string `json:"ref,omitempty"`
	SHA            string `json:"sha,omitempty"`
	Disposition    string `json:"disposition"`
	IngestedAt     string `json:"ingested_at"` // RFC3339Nano: when dispatch completed
	SentAt         string `json:"sent_at"`     // RFC3339Nano: stamped per attempt
}

// identifiers are the cheap per-event correlation fields lifted from the
// delivery payload. Absent fields stay zero and are omitted from the JSON.
type identifiers struct {
	prNumber int64
	ref      string
	sha      string
}

// extractIdentifiers pulls the correlation identifiers the event type
// carries: PR events yield pr_number + head sha, pushes yield ref + after
// sha, CI events yield the commit sha. Anything else includes what is cheaply
// available and omits the rest; a malformed payload just yields fewer fields.
func extractIdentifiers(event webhook.Event) identifiers {
	var ids identifiers
	switch event.Type {
	case "pull_request", "pull_request_review":
		ids.prNumber = event.PRNumber
		var body struct {
			PullRequest *struct {
				Head *struct {
					SHA string `json:"sha"`
				} `json:"head"`
			} `json:"pull_request"`
		}
		if json.Unmarshal(event.Raw, &body) == nil && body.PullRequest != nil && body.PullRequest.Head != nil {
			ids.sha = body.PullRequest.Head.SHA
		}
	case "push":
		var body struct {
			Ref   string `json:"ref"`
			After string `json:"after"`
		}
		if json.Unmarshal(event.Raw, &body) == nil {
			ids.ref = body.Ref
			ids.sha = body.After
		}
	case "status":
		var body struct {
			SHA string `json:"sha"`
		}
		if json.Unmarshal(event.Raw, &body) == nil {
			ids.sha = body.SHA
		}
	case "check_run":
		var body struct {
			CheckRun *struct {
				HeadSHA string `json:"head_sha"`
			} `json:"check_run"`
		}
		if json.Unmarshal(event.Raw, &body) == nil && body.CheckRun != nil {
			ids.sha = body.CheckRun.HeadSHA
		}
	case "check_suite":
		var body struct {
			CheckSuite *struct {
				HeadSHA string `json:"head_sha"`
			} `json:"check_suite"`
		}
		if json.Unmarshal(event.Raw, &body) == nil && body.CheckSuite != nil {
			ids.sha = body.CheckSuite.HeadSHA
		}
	case "workflow_job":
		var body struct {
			WorkflowJob *struct {
				HeadSHA string `json:"head_sha"`
			} `json:"workflow_job"`
		}
		if json.Unmarshal(event.Raw, &body) == nil && body.WorkflowJob != nil {
			ids.sha = body.WorkflowJob.HeadSHA
		}
	}
	return ids
}
