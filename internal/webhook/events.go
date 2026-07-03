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
	Owner           string
	Repo            string
	SHA             string
	Context         string
	State           string // normalized: SUCCESS / FAILURE / ERROR / PENDING
	OnDefaultBranch bool   // the check ran on the repo's default branch
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

	var branches []string
	switch eventType {
	case "status":
		p.SHA = body.SHA
		p.Context = "status:" + body.Context
		p.State = normalizeStatusState(body.State)
		for _, b := range body.Branches {
			branches = append(branches, b.Name)
		}
	case "check_run":
		if body.CheckRun == nil {
			return CheckPayload{}, fmt.Errorf("parse check_run payload: no check_run field")
		}
		p.SHA = body.CheckRun.HeadSHA
		p.Context = "check_run:" + body.CheckRun.Name
		p.State = normalizeCheckState(body.CheckRun.Status, body.CheckRun.Conclusion)
		if body.CheckRun.CheckSuite != nil {
			branches = append(branches, body.CheckRun.CheckSuite.HeadBranch)
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
		branches = append(branches, body.CheckSuite.HeadBranch)
	default:
		return CheckPayload{}, fmt.Errorf("unsupported check event type: %s", eventType)
	}

	if defaultBranch != "" {
		for _, b := range branches {
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

// PushPayload is the minimal info applied directly from a push webhook.
type PushPayload struct {
	Owner    string
	Repo     string
	PushedAt string // RFC3339
	Ref      string // the pushed ref, e.g. "refs/heads/main" ("" when absent)

	// Fields for absorbing the pushed commits into the git-commits cache.
	Before  string // sha of the ref before the push (all-zeros for a new ref)
	After   string // sha of the ref after the push
	Forced  bool
	Commits []PushCommit // pushed commits, payload order (oldest first)
}

// Branch returns the branch name for a refs/heads/* push, or "" for tag
// pushes and other refs.
func (p PushPayload) Branch() string {
	const prefix = "refs/heads/"
	if len(p.Ref) > len(prefix) && p.Ref[:len(prefix)] == prefix {
		return p.Ref[len(prefix):]
	}
	return ""
}

// PushCommit is one commit object from a push payload. The payload states the
// commit's id, tree id, message, timestamp, and author/committer identities --
// exactly the state GET /repos/{o}/{r}/git/commits/{sha} returns -- but NOT
// its parents; those are derived (see ChainedCommits).
type PushCommit struct {
	ID             string
	TreeID         string
	Message        string
	Timestamp      string // RFC3339 (normalised)
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
}

// ParsePushPayload extracts owner/repo, a best-effort pushed_at timestamp, and
// the pushed commits (for git-commit cache absorption).
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
		Ref     string `json:"ref"`
		Before  string `json:"before"`
		After   string `json:"after"`
		Forced  bool   `json:"forced"`
		Commits []struct {
			ID        string `json:"id"`
			TreeID    string `json:"tree_id"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
			Author    struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"committer"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return PushPayload{}, fmt.Errorf("parse push payload: %w", err)
	}
	if body.Repository == nil {
		return PushPayload{}, fmt.Errorf("parse push payload: no repository field")
	}
	p := PushPayload{
		Owner:  body.Repository.Owner.Login,
		Repo:   body.Repository.Name,
		Ref:    body.Ref,
		Before: body.Before,
		After:  body.After,
		Forced: body.Forced,
	}
	if body.HeadCommit != nil && body.HeadCommit.Timestamp != "" {
		p.PushedAt = normaliseTime(body.HeadCommit.Timestamp)
	} else {
		p.PushedAt = time.Now().UTC().Format(time.RFC3339)
	}
	for _, c := range body.Commits {
		p.Commits = append(p.Commits, PushCommit{
			ID:             c.ID,
			TreeID:         c.TreeID,
			Message:        c.Message,
			Timestamp:      normaliseTime(c.Timestamp),
			AuthorName:     c.Author.Name,
			AuthorEmail:    c.Author.Email,
			CommitterName:  c.Committer.Name,
			CommitterEmail: c.Committer.Email,
		})
	}
	if p.Owner == "" || p.Repo == "" {
		return PushPayload{}, fmt.Errorf("parse push payload: missing owner/repo")
	}
	return p, nil
}

// maxChainedPushCommits is the payload size at which parent derivation stops
// trusting the commits array: GitHub caps the array (larger pushes are
// truncated), and a truncated array breaks the before -> commits[0] chain.
const maxChainedPushCommits = 20

// ChainedCommits returns the pushed commits with a trustworthy linear parent
// chain -- commits[0]'s parent is `before`, each subsequent commit's parent is
// its predecessor -- or nil when the derivation cannot be trusted: a forced
// push (before is not the parent), a new ref (before is all zeros), a
// possibly-truncated array, or an array whose last id is not `after`
// (non-linear ordering). A pushed MERGE commit is the one case the payload
// cannot reveal (its extra parents are simply absent from the chain); the real
// consumer of cached parents -- pr-minder's test-merge inspection -- only
// reads parents on refs/pull/N/merge commits, which never arrive via push
// payloads (always fetch-sourced). So the chain is absorbed for the common
// linear push and dropped whenever any signal says otherwise.
func (p PushPayload) ChainedCommits() []PushCommit {
	n := len(p.Commits)
	if n == 0 || n >= maxChainedPushCommits || p.Forced {
		return nil
	}
	if !isRealSHA(p.Before) || p.Commits[n-1].ID != p.After {
		return nil
	}
	out := make([]PushCommit, n)
	copy(out, p.Commits)
	return out
}

// ParentForChained returns the derived parent sha for the i-th commit of a
// ChainedCommits slice: `before` for the first, the previous commit otherwise.
func (p PushPayload) ParentForChained(chain []PushCommit, i int) string {
	if i == 0 {
		return p.Before
	}
	return chain[i-1].ID
}

// isRealSHA reports whether s is a non-zero full-length hex object id (the
// all-zeros sha marks a created ref, which has no before-parent).
func isRealSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	real := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
		if c != '0' {
			real = true
		}
	}
	return real
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
