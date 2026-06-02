package webhook

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

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

// PRPayload holds the full PR data and labels parsed from a webhook payload.
type PRPayload struct {
	PR     dbgen.PullRequest
	Labels []dbgen.PrLabel
}

// ParsePRPayload extracts a full PR and its labels from a pull_request webhook's
// raw JSON. The Actor field is left empty — callers fill it per-actor.
func ParsePRPayload(raw json.RawMessage) (PRPayload, error) {
	var body struct {
		PullRequest *struct {
			Number    int    `json:"number"`
			Title     string `json:"title"`
			HTMLURL   string `json:"html_url"`
			Draft     bool   `json:"draft"`
			State     string `json:"state"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
			Additions *int   `json:"additions"`
			Deletions *int   `json:"deletions"`
			Mergeable *bool  `json:"mergeable"`
			User      *struct {
				Login     string `json:"login"`
				AvatarURL string `json:"avatar_url"`
				HTMLURL   string `json:"html_url"`
			} `json:"user"`
			Head struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				Ref  string `json:"ref"`
				Repo *struct {
					Name  string `json:"name"`
					Owner struct {
						Login string `json:"login"`
					} `json:"owner"`
				} `json:"repo"`
			} `json:"base"`
			Labels []struct {
				Name  string `json:"name"`
				Color string `json:"color"`
			} `json:"labels"`
			RequestedReviewers []json.RawMessage `json:"requested_reviewers"`
			RequestedTeams     []json.RawMessage `json:"requested_teams"`
		} `json:"pull_request"`
	}

	if err := json.Unmarshal(raw, &body); err != nil {
		return PRPayload{}, fmt.Errorf("parse PR webhook payload: %w", err)
	}
	if body.PullRequest == nil {
		return PRPayload{}, fmt.Errorf("parse PR webhook payload: no pull_request field")
	}
	gpr := body.PullRequest

	// Derive owner/repo from base.repo (always present for PR webhooks).
	var owner, repo string
	if gpr.Base.Repo != nil {
		owner = gpr.Base.Repo.Owner.Login
		repo = gpr.Base.Repo.Name
	}

	// Map REST state to the UPPER format used by the GraphQL-origin cache.
	state := "OPEN"
	switch gpr.State {
	case "closed":
		state = "CLOSED"
	case "open":
		state = "OPEN"
	}

	// Normalise timestamps to RFC3339 (GitHub REST already sends them this way).
	createdAt := normaliseTime(gpr.CreatedAt)
	updatedAt := normaliseTime(gpr.UpdatedAt)

	pr := dbgen.PullRequest{
		Owner:       owner,
		Repo:        repo,
		Number:      int64(gpr.Number),
		Title:       gpr.Title,
		Url:         gpr.HTMLURL,
		IsDraft:     boolToInt(gpr.Draft),
		State:       state,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		HeadRefName: nullStr(gpr.Head.Ref),
		BaseRefName: nullStr(gpr.Base.Ref),
		HeadRefOid:  nullStr(gpr.Head.SHA),
	}

	if gpr.Additions != nil {
		pr.Additions = sql.NullInt64{Int64: int64(*gpr.Additions), Valid: true}
	}
	if gpr.Deletions != nil {
		pr.Deletions = sql.NullInt64{Int64: int64(*gpr.Deletions), Valid: true}
	}
	if gpr.Mergeable != nil {
		m := "UNKNOWN"
		if *gpr.Mergeable {
			m = "MERGEABLE"
		} else {
			m = "CONFLICTING"
		}
		pr.Mergeable = sql.NullString{String: m, Valid: true}
	}
	if gpr.User != nil {
		pr.AuthorLogin = nullStr(gpr.User.Login)
		pr.AuthorAvatar = nullStr(gpr.User.AvatarURL)
		pr.AuthorUrl = nullStr(gpr.User.HTMLURL)
	}

	reviewCount := len(gpr.RequestedReviewers) + len(gpr.RequestedTeams)
	pr.ReviewRequestCount = sql.NullInt64{Int64: int64(reviewCount), Valid: true}

	var labels []dbgen.PrLabel
	for _, l := range gpr.Labels {
		labels = append(labels, dbgen.PrLabel{
			Owner:    owner,
			Repo:     repo,
			PrNumber: int64(gpr.Number),
			Name:     l.Name,
			Color:    l.Color,
		})
	}

	return PRPayload{PR: pr, Labels: labels}, nil
}

// CheckPayload is a single commit-check state parsed from a status/check_run/
// check_suite webhook. Context is a stable dedup key (latest state wins).
type CheckPayload struct {
	Owner   string
	Repo    string
	SHA     string
	Context string
	State   string // normalized: SUCCESS / FAILURE / ERROR / PENDING
}

// ParseCheckPayload extracts a commit-check state from a status, check_run, or
// check_suite webhook payload.
func ParseCheckPayload(eventType string, raw json.RawMessage) (CheckPayload, error) {
	var body struct {
		SHA      string `json:"sha"`
		State    string `json:"state"`
		Context  string `json:"context"`
		CheckRun *struct {
			HeadSHA    string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Name       string `json:"name"`
		} `json:"check_run"`
		CheckSuite *struct {
			HeadSHA    string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			App        *struct {
				Slug string `json:"slug"`
			} `json:"app"`
		} `json:"check_suite"`
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return CheckPayload{}, fmt.Errorf("parse check webhook payload: %w", err)
	}

	var p CheckPayload
	if body.Repository != nil {
		p.Owner = body.Repository.Owner.Login
		p.Repo = body.Repository.Name
	}

	switch eventType {
	case "status":
		p.SHA = body.SHA
		p.Context = "status:" + body.Context
		p.State = normalizeStatusState(body.State)
	case "check_run":
		if body.CheckRun == nil {
			return CheckPayload{}, fmt.Errorf("parse check_run payload: no check_run field")
		}
		p.SHA = body.CheckRun.HeadSHA
		p.Context = "check_run:" + body.CheckRun.Name
		p.State = normalizeCheckState(body.CheckRun.Status, body.CheckRun.Conclusion)
	case "check_suite":
		if body.CheckSuite == nil {
			return CheckPayload{}, fmt.Errorf("parse check_suite payload: no check_suite field")
		}
		p.SHA = body.CheckSuite.HeadSHA
		slug := ""
		if body.CheckSuite.App != nil {
			slug = body.CheckSuite.App.Slug
		}
		p.Context = "check_suite:" + slug
		p.State = normalizeCheckState(body.CheckSuite.Status, body.CheckSuite.Conclusion)
	default:
		return CheckPayload{}, fmt.Errorf("unsupported check event type: %s", eventType)
	}

	if p.Owner == "" || p.Repo == "" || p.SHA == "" {
		return CheckPayload{}, fmt.Errorf("parse check payload: missing owner/repo/sha")
	}
	return p, nil
}

func normalizeStatusState(state string) string {
	switch state {
	case "success":
		return "SUCCESS"
	case "pending":
		return "PENDING"
	case "failure":
		return "FAILURE"
	case "error":
		return "ERROR"
	}
	return "PENDING"
}

func normalizeCheckState(status, conclusion string) string {
	if status != "completed" {
		return "PENDING"
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return "SUCCESS"
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
		return "FAILURE"
	}
	return "PENDING"
}

// PushPayload is the minimal info applied directly from a push webhook.
type PushPayload struct {
	Owner    string
	Repo     string
	PushedAt string // RFC3339
}

// ParsePushPayload extracts owner/repo and a best-effort pushed_at timestamp.
func ParsePushPayload(raw json.RawMessage) (PushPayload, error) {
	var body struct {
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		HeadCommit *struct {
			Timestamp string `json:"timestamp"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return PushPayload{}, fmt.Errorf("parse push payload: %w", err)
	}
	if body.Repository == nil {
		return PushPayload{}, fmt.Errorf("parse push payload: no repository field")
	}
	p := PushPayload{
		Owner: body.Repository.Owner.Login,
		Repo:  body.Repository.Name,
	}
	if body.HeadCommit != nil && body.HeadCommit.Timestamp != "" {
		p.PushedAt = normaliseTime(body.HeadCommit.Timestamp)
	} else {
		p.PushedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if p.Owner == "" || p.Repo == "" {
		return PushPayload{}, fmt.Errorf("parse push payload: missing owner/repo")
	}
	return p, nil
}

// LabelPayload is a repo label change parsed from a label webhook.
type LabelPayload struct {
	Owner   string
	Repo    string
	Action  string
	Name    string
	Color   string
	OldName string // changes.name.from, for renames
}

// ParseLabelPayload extracts a repo-level label change.
func ParseLabelPayload(raw json.RawMessage) (LabelPayload, error) {
	var body struct {
		Action string `json:"action"`
		Label  *struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		} `json:"label"`
		Changes *struct {
			Name *struct {
				From string `json:"from"`
			} `json:"name"`
		} `json:"changes"`
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return LabelPayload{}, fmt.Errorf("parse label payload: %w", err)
	}
	if body.Label == nil || body.Repository == nil {
		return LabelPayload{}, fmt.Errorf("parse label payload: missing label/repository")
	}
	p := LabelPayload{
		Owner:  body.Repository.Owner.Login,
		Repo:   body.Repository.Name,
		Action: body.Action,
		Name:   body.Label.Name,
		Color:  body.Label.Color,
	}
	if body.Changes != nil && body.Changes.Name != nil {
		p.OldName = body.Changes.Name.From
	}
	if p.Owner == "" || p.Repo == "" {
		return LabelPayload{}, fmt.Errorf("parse label payload: missing owner/repo")
	}
	return p, nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func normaliseTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}
