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

	// GitHub App installation that produced this delivery (0 when absent). Used
	// to pull an as-yet-uncached repo on demand, as that installation.
	InstallationID int64

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
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  *bool  `json:"private"`
	// Visibility is "public" / "private" / "internal"; older payloads may omit
	// it, in which case Private decides.
	Visibility    string  `json:"visibility"`
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

// repoVisibility folds the payload's visibility/private pair into the stored
// value: the explicit visibility field wins ("internal" is kept as-is and is
// NOT public for the reveal fast path); absent both, unknown.
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

// PRPayload holds the full PR data and labels parsed from a webhook payload.
type PRPayload struct {
	PR     dbgen.PullRequest
	Labels []dbgen.PrLabel
}

// ParsePRPayload extracts a full PR and its labels from a pull_request
// webhook's raw JSON (the embedded pull_request is a full REST-shaped PR
// object, so webhook-maintained rows stay rest-complete for the cached
// /pulls routes).
func ParsePRPayload(raw json.RawMessage) (PRPayload, error) {
	var body struct {
		PullRequest *struct {
			Number    int     `json:"number"`
			NodeID    string  `json:"node_id"`
			Title     string  `json:"title"`
			Body      *string `json:"body"`
			HTMLURL   string  `json:"html_url"`
			Draft     bool    `json:"draft"`
			State     string  `json:"state"`
			CreatedAt string  `json:"created_at"`
			UpdatedAt string  `json:"updated_at"`
			Additions *int    `json:"additions"`
			Deletions *int    `json:"deletions"`
			Mergeable *bool   `json:"mergeable"`
			User      *struct {
				Login     string `json:"login"`
				Type      string `json:"type"`
				AvatarURL string `json:"avatar_url"`
				HTMLURL   string `json:"html_url"`
			} `json:"user"`
			Head struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo *struct {
					Name  string `json:"name"`
					Owner struct {
						Login string `json:"login"`
					} `json:"owner"`
				} `json:"repo"`
			} `json:"base"`
			AutoMerge *struct {
				MergeMethod string `json:"merge_method"`
			} `json:"auto_merge"`
			MergeCommitSHA *string `json:"merge_commit_sha"`
			Labels         []struct {
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
		// REST-only fields (absent from the GraphQL org-repos selection set)
		// that the cached /pulls routes rebuild from. Webhook payloads carry
		// them all, so webhook-maintained rows stay rebuild-complete.
		NodeID:     nullStr(gpr.NodeID),
		BaseRefOid: nullStr(gpr.Base.SHA),
	}
	if gpr.Body != nil {
		pr.Body = sql.NullString{String: *gpr.Body, Valid: true}
	}
	if gpr.Head.Repo != nil {
		pr.HeadRepoFullName = nullStr(gpr.Head.Repo.FullName)
	}
	if gpr.AutoMerge != nil {
		pr.AutoMergeMethod = nullStr(gpr.AutoMerge.MergeMethod)
	}
	if gpr.MergeCommitSHA != nil {
		pr.MergeCommitSha = nullStr(*gpr.MergeCommitSHA)
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
		pr.AuthorType = nullStr(gpr.User.Type)
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
	// Branches is every branch name the payload associates with the commit
	// (empty names dropped): for a `status` event each branches[].name; for
	// check_run/check_suite the suite's head_branch when non-empty. Together
	// with SHA these are the ref SPELLINGS whose cached CI answers the event
	// moved (commit_ci_cache keys the verbatim requested ref). NOTE: GitHub
	// caps the status payload's branches array (~10 entries), so a commit on
	// many branches can be under-reported -- acceptable, because branch-form
	// CI rows are bounded by the 24h TTL and current consumers poll by sha.
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

// WorkflowJobPayload is a GitHub Actions job's state parsed from a
// workflow_job webhook. Only the in_progress and completed actions are
// tracked (the dispatcher drops queued/waiting churn); this struct carries just
// what the global workflow_jobs table stores. Empty string means the payload
// didn't report the field (e.g. Conclusion until completed, RunnerName until a
// runner is assigned).
type WorkflowJobPayload struct {
	Owner        string
	Repo         string
	JobID        int64
	RunID        int64
	RunAttempt   int64
	Name         string
	WorkflowName string
	Status       string // in_progress | completed
	Conclusion   string // success | failure | cancelled | ... (completed only)
	HeadSHA      string
	HeadBranch   string
	HTMLURL      string
	StartedAt    string // RFC3339
	CompletedAt  string // RFC3339
	RunnerName   string
}

// ParseWorkflowJobPayload extracts a job's state from a workflow_job webhook.
func ParseWorkflowJobPayload(raw json.RawMessage) (WorkflowJobPayload, error) {
	var body struct {
		WorkflowJob *struct {
			ID           int64   `json:"id"`
			RunID        int64   `json:"run_id"`
			RunAttempt   int64   `json:"run_attempt"`
			Name         string  `json:"name"`
			WorkflowName *string `json:"workflow_name"`
			Status       string  `json:"status"`
			Conclusion   *string `json:"conclusion"`
			HeadSHA      string  `json:"head_sha"`
			HeadBranch   *string `json:"head_branch"`
			HTMLURL      string  `json:"html_url"`
			StartedAt    *string `json:"started_at"`
			CompletedAt  *string `json:"completed_at"`
			RunnerName   *string `json:"runner_name"`
		} `json:"workflow_job"`
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return WorkflowJobPayload{}, fmt.Errorf("parse workflow_job payload: %w", err)
	}
	if body.WorkflowJob == nil || body.Repository == nil {
		return WorkflowJobPayload{}, fmt.Errorf("parse workflow_job payload: missing workflow_job/repository")
	}
	j := body.WorkflowJob
	p := WorkflowJobPayload{
		Owner:        body.Repository.Owner.Login,
		Repo:         body.Repository.Name,
		JobID:        j.ID,
		RunID:        j.RunID,
		RunAttempt:   j.RunAttempt,
		Name:         j.Name,
		WorkflowName: strOrEmpty(j.WorkflowName),
		Status:       j.Status,
		Conclusion:   strOrEmpty(j.Conclusion),
		HeadSHA:      j.HeadSHA,
		HeadBranch:   strOrEmpty(j.HeadBranch),
		HTMLURL:      j.HTMLURL,
		RunnerName:   strOrEmpty(j.RunnerName),
	}
	if ts := strOrEmpty(j.StartedAt); ts != "" {
		p.StartedAt = normaliseTime(ts)
	}
	if ts := strOrEmpty(j.CompletedAt); ts != "" {
		p.CompletedAt = normaliseTime(ts)
	}
	if p.Owner == "" || p.Repo == "" || p.JobID == 0 {
		return WorkflowJobPayload{}, fmt.Errorf("parse workflow_job payload: missing owner/repo/job id")
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
