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
