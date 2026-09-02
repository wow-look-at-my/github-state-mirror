package webhook

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Event is a parsed webhook event with just enough info for dispatch.
type Event struct {
	Type       string // X-GitHub-Event header value
	DeliveryID string // X-GitHub-Delivery header value (UUID), for the delivery log
	Action     string // "action" field from payload

	// Repository info (extracted from payload.repository)
	RepoOwnerLogin string
	RepoNameStr    string

	// PR info (extracted from payload.pull_request if present)
	PRNumber int64
	PRBase   string
	PRHead   string

	// Org info
	OrgLogin string

	InstallationID int64 // the delivering App installation; when absent

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
		Action     string `json:"action"`
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
		Installation *struct {
			ID int64 `json:"id"`
		} `json:"installation"`
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
	if body.Installation != nil {
		e.InstallationID = body.Installation.ID
	}

	return e
}

// repositoryObject is the payload's embedded repository object, carrying the
// fields global truth keeps. Webhook payloads (unlike the identity-locked
// GraphQL org query) DO carry visibility, so this is the reveal layer's main
// source of public/private truth.
type repositoryObject struct {
	Name          string  `json:"name"`
	FullName      string  `json:"full_name"`
	Private       *bool   `json:"private"`
	Visibility    string  `json:"visibility"` // public/private/internal; absent falls back to Private
	HTMLURL       string  `json:"html_url"`
	DefaultBranch string  `json:"default_branch"`
	PushedAt      any     `json:"pushed_at"` // RFC3339 string, or unix seconds on some events
	Archived      bool    `json:"archived"`
	Disabled      bool    `json:"disabled"`
	Fork          bool    `json:"fork"`
	Description   *string `json:"description"`
	Owner         struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
		HTMLURL   string `json:"html_url"`
	} `json:"owner"`
}

// toRepo converts the payload object into a truth row. Fields the payload does
// not state stay NULL/'' so the store's COALESCE upsert preserves anything
// already known.
func (r *repositoryObject) toRepo() (dbgen.Repo, bool) {
	if r == nil || r.Name == "" || r.Owner.Login == "" {
		return dbgen.Repo{}, false
	}
	out := dbgen.Repo{
		Owner:         r.Owner.Login,
		Name:          r.Name,
		NameWithOwner: r.FullName,
		Url:           r.HTMLURL,
		IsArchived:    boolToInt(r.Archived),
		IsDisabled:    boolToInt(r.Disabled),
		Visibility:    repoVisibility(r.Visibility, r.Private),
		OwnerLogin:    nullStr(r.Owner.Login),
		OwnerAvatar:   nullStr(r.Owner.AvatarURL),
		OwnerUrl:      nullStr(r.Owner.HTMLURL),
	}
	if out.NameWithOwner == "" {
		out.NameWithOwner = r.Owner.Login + "/" + r.Name
	}
	if r.DefaultBranch != "" {
		out.DefaultBranch = nullStr(r.DefaultBranch)
	}
	if ts := timestampString(r.PushedAt); ts != "" {
		out.PushedAt = nullStr(ts)
	}
	return out, true
}

// repoVisibility: explicit visibility wins; "internal" is NOT public.
func repoVisibility(visibility string, private *bool) string {
	if visibility != "" {
		return visibility
	}
	if private == nil {
		return ""
	}
	if *private {
		return "private"
	}
	return "public"
}

// timestampString renders a payload timestamp that may be an RFC3339 string or
// a unix-seconds number (push events use the latter for repository.pushed_at).
func timestampString(v any) string {
	switch t := v.(type) {
	case string:
		return normaliseTime(t)
	case float64:
		if t <= 0 {
			return ""
		}
		return time.Unix(int64(t), 0).UTC().Format(time.RFC3339)
	default:
		return ""
	}
}

// ParseRepositoryPayload extracts the payload's embedded repository object as
// a truth row, reporting false when the payload has none (or it is degenerate).
func ParseRepositoryPayload(raw json.RawMessage) (dbgen.Repo, bool) {
	var body struct {
		Repository *repositoryObject `json:"repository"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Repository == nil {
		return dbgen.Repo{}, false
	}
	return body.Repository.toRepo()
}

// ParseRepositoryObject parses a BARE repository object (e.g. the body of
// GET /repos/{owner}/{repo} -- the reveal probe's answer) into a truth row.
func ParseRepositoryObject(raw []byte) (dbgen.Repo, bool) {
	var obj repositoryObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return dbgen.Repo{}, false
	}
	return obj.toRepo()
}

// ParseInstallationAppID returns the app_id of the delivery's own
// `installation` object ("" when absent or unparseable) -- an
// `installation`/`installation_repositories` delivery's app-installations
// cache flush target, since GitHub's installation object carries the owning
// App's id directly.
func ParseInstallationAppID(raw json.RawMessage) string {
	var body struct {
		Installation *struct {
			AppID int64 `json:"app_id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Installation == nil || body.Installation.AppID <= 0 {
		return ""
	}
	return strconv.FormatInt(body.Installation.AppID, 10)
}

// ParseRenameFrom returns changes.repository.name.from for a repository
// renamed event ("" when absent).
func ParseRenameFrom(raw json.RawMessage) string {
	var body struct {
		Changes *struct {
			Repository *struct {
				Name *struct {
					From string `json:"from"`
				} `json:"name"`
			} `json:"repository"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Changes == nil ||
		body.Changes.Repository == nil || body.Changes.Repository.Name == nil {
		return ""
	}
	return body.Changes.Repository.Name.From
}

// CheckPayload is a single commit-check state parsed from a status/check_run/
// check_suite webhook. Context is a stable dedup key (latest state wins).
type CheckPayload struct {
	Owner   string
	Repo    string
	SHA     string
	Context string
	State   string // normalized: SUCCESS / FAILURE / ERROR / PENDING
	// Branches is every branch name the payload associates with the commit
	Branches        []string
	OnDefaultBranch bool // the check ran on the repo's default branch
}

// ParseCheckPayload extracts a commit-check state from a status, check_run, or
// check_suite webhook payload.
func ParseCheckPayload(eventType string, raw json.RawMessage) (CheckPayload, error) {
	var body struct {
		SHA      string `json:"sha"`
		State    string `json:"state"`
		Context  string `json:"context"`
		Branches []struct {
			Name string `json:"name"`
		} `json:"branches"`
		CheckRun *struct {
			HeadSHA    string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Name       string `json:"name"`
			CheckSuite *struct {
				HeadBranch string `json:"head_branch"`
			} `json:"check_suite"`
		} `json:"check_run"`
		CheckSuite *struct {
			HeadSHA    string `json:"head_sha"`
			HeadBranch string `json:"head_branch"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			App        *struct {
				Slug string `json:"slug"`
			} `json:"app"`
		} `json:"check_suite"`
		Repository *struct {
			Name          string `json:"name"`
			DefaultBranch string `json:"default_branch"`
			Owner         struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return CheckPayload{}, fmt.Errorf("parse check webhook payload: %w", err)
	}

	var p CheckPayload
	defaultBranch := ""
	if body.Repository != nil {
		p.Owner = body.Repository.Owner.Login
		p.Repo = body.Repository.Name
		defaultBranch = body.Repository.DefaultBranch
	}

	// p.Branches collects only NON-EMPTY names; the OnDefaultBranch check
	// below reads the same list, which is behavior-identical to the old
	// unfiltered local slice (an empty name can never equal a non-empty
	// default branch).
	switch eventType {
	case "status":
		p.SHA = body.SHA
		p.Context = "status:" + body.Context
		p.State = normalizeStatusState(body.State)
		for _, b := range body.Branches {
			if b.Name != "" {
				p.Branches = append(p.Branches, b.Name)
			}
		}
	case "check_run":
		if body.CheckRun == nil {
			return CheckPayload{}, fmt.Errorf("parse check_run payload: no check_run field")
		}
		p.SHA = body.CheckRun.HeadSHA
		p.Context = "check_run:" + body.CheckRun.Name
		p.State = normalizeCheckState(body.CheckRun.Status, body.CheckRun.Conclusion)
		if body.CheckRun.CheckSuite != nil && body.CheckRun.CheckSuite.HeadBranch != "" {
			p.Branches = append(p.Branches, body.CheckRun.CheckSuite.HeadBranch)
		}
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
		if body.CheckSuite.HeadBranch != "" {
			p.Branches = append(p.Branches, body.CheckSuite.HeadBranch)
		}
	default:
		return CheckPayload{}, fmt.Errorf("unsupported check event type: %s", eventType)
	}

	if defaultBranch != "" {
		for _, b := range p.Branches {
			if b == defaultBranch {
				p.OnDefaultBranch = true
				break
			}
		}
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

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
