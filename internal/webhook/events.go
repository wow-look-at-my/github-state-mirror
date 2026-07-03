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

// RESTPullRequest is GitHub's REST pull-request object shape, shared by the
// pull_request webhook payload (payload.pull_request IS a REST PR object) and
// the cached /pulls routes' upstream fetches. Pointer fields distinguish a
// JSON null (carried, empty -> stored '') from the field being consumed by a
// source that omits it entirely (never happens for REST; the GraphQL absorb
// path leaves the corresponding columns NULL = unknown).
type RESTPullRequest struct {
	Number         int             `json:"number"`
	NodeID         string          `json:"node_id"`
	Title          string          `json:"title"`
	Body           *string         `json:"body"`
	HTMLURL        string          `json:"html_url"`
	Draft          bool            `json:"draft"`
	State          string          `json:"state"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	Additions      *int            `json:"additions"`
	Deletions      *int            `json:"deletions"`
	Mergeable      *bool           `json:"mergeable"`
	MergeableState *string         `json:"mergeable_state"`
	MergeCommitSHA *string         `json:"merge_commit_sha"`
	AutoMerge      json.RawMessage `json:"auto_merge"`
	User           *struct {
		Login     string `json:"login"`
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
		Ref  string            `json:"ref"`
		SHA  string            `json:"sha"`
		Repo *repositoryObject `json:"repo"`
	} `json:"base"`
	Labels []struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"labels"`
	RequestedReviewers []json.RawMessage `json:"requested_reviewers"`
	RequestedTeams     []json.RawMessage `json:"requested_teams"`
}

// hasDetailFields reports whether this object carries the single-PR-only
// fields (mergeable/mergeable_state/additions/deletions). The REST LIST
// endpoint omits them; the single-PR GET and webhook payloads carry them.
// Detection is by mergeable_state, which the detailed shape always includes
// (as a string) and the list shape never does.
func (g *RESTPullRequest) hasDetailFields() bool { return g.MergeableState != nil }

// ToPullRequest converts a REST PR object into a truth row (plus its labels).
// ownerFallback/repoFallback are used when base.repo is absent (the /pulls
// routes know them from the URL; webhook payloads carry base.repo).
func (g *RESTPullRequest) ToPullRequest(ownerFallback, repoFallback string) (dbgen.PullRequest, []dbgen.PrLabel, error) {
	owner, repo := ownerFallback, repoFallback
	if g.Base.Repo != nil && g.Base.Repo.Name != "" && g.Base.Repo.Owner.Login != "" {
		owner = g.Base.Repo.Owner.Login
		repo = g.Base.Repo.Name
	}
	if owner == "" || repo == "" || g.Number == 0 {
		return dbgen.PullRequest{}, nil, fmt.Errorf("parse REST pull request: missing owner/repo/number")
	}

	// Map REST state to the UPPER format used across the truth store.
	state := "OPEN"
	if g.State == "closed" {
		state = "CLOSED"
	}

	pr := dbgen.PullRequest{
		Owner:       owner,
		Repo:        repo,
		Number:      int64(g.Number),
		Title:       g.Title,
		Url:         g.HTMLURL,
		IsDraft:     boolToInt(g.Draft),
		State:       state,
		CreatedAt:   normaliseTime(g.CreatedAt),
		UpdatedAt:   normaliseTime(g.UpdatedAt),
		HeadRefName: nullStr(g.Head.Ref),
		BaseRefName: nullStr(g.Base.Ref),
		HeadRefOid:  nullStr(g.Head.SHA),
		// REST-complete fields. JSON null maps to '' ("carried and empty") so
		// the COALESCE upsert distinguishes it from NULL ("source doesn't
		// carry it", the GraphQL absorb path).
		NodeID:           sql.NullString{String: g.NodeID, Valid: true},
		Body:             sql.NullString{String: strOrEmpty(g.Body), Valid: true},
		MergeCommitSha:   sql.NullString{String: strOrEmpty(g.MergeCommitSHA), Valid: true},
		BaseSha:          sql.NullString{String: g.Base.SHA, Valid: true},
		AutoMerge:        sql.NullString{String: trimmedAutoMerge(g.AutoMerge), Valid: true},
		HeadRepoFullName: sql.NullString{String: "", Valid: true},
	}
	if g.Head.Repo != nil {
		pr.HeadRepoFullName.String = g.Head.Repo.FullName
	}
	if g.MergeableState != nil {
		pr.MergeableState = sql.NullString{String: *g.MergeableState, Valid: true}
	}
	if g.Additions != nil {
		pr.Additions = sql.NullInt64{Int64: int64(*g.Additions), Valid: true}
	}
	if g.Deletions != nil {
		pr.Deletions = sql.NullInt64{Int64: int64(*g.Deletions), Valid: true}
	}
	if g.Mergeable != nil {
		m := "CONFLICTING"
		if *g.Mergeable {
			m = "MERGEABLE"
		}
		pr.Mergeable = sql.NullString{String: m, Valid: true}
	}
	if g.User != nil {
		pr.AuthorLogin = nullStr(g.User.Login)
		pr.AuthorAvatar = nullStr(g.User.AvatarURL)
		pr.AuthorUrl = nullStr(g.User.HTMLURL)
	}
	reviewCount := len(g.RequestedReviewers) + len(g.RequestedTeams)
	pr.ReviewRequestCount = sql.NullInt64{Int64: int64(reviewCount), Valid: true}

	var labels []dbgen.PrLabel
	for _, l := range g.Labels {
		labels = append(labels, dbgen.PrLabel{
			Owner:    owner,
			Repo:     repo,
			PrNumber: int64(g.Number),
			Name:     l.Name,
			Color:    l.Color,
		})
	}
	return pr, labels, nil
}

// trimmedAutoMerge reduces GitHub's auto_merge object to the state fields
// consumers read, dropping the enabled_by user's URL clutter. A JSON null (not
// armed) becomes '' -- carried-and-empty.
func trimmedAutoMerge(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var am struct {
		EnabledBy *struct {
			Login string `json:"login"`
		} `json:"enabled_by"`
		MergeMethod   string  `json:"merge_method"`
		CommitTitle   *string `json:"commit_title"`
		CommitMessage *string `json:"commit_message"`
	}
	if err := json.Unmarshal(raw, &am); err != nil {
		return ""
	}
	out := map[string]any{
		"merge_method":   am.MergeMethod,
		"commit_title":   am.CommitTitle,
		"commit_message": am.CommitMessage,
	}
	if am.EnabledBy != nil {
		out["enabled_by"] = map[string]any{"login": am.EnabledBy.Login}
	} else {
		out["enabled_by"] = nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParsePRPayload extracts a full PR and its labels from a pull_request
// webhook's raw JSON (the embedded pull_request IS a REST PR object).
func ParsePRPayload(raw json.RawMessage) (PRPayload, error) {
	var body struct {
		PullRequest *RESTPullRequest `json:"pull_request"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return PRPayload{}, fmt.Errorf("parse PR webhook payload: %w", err)
	}
	if body.PullRequest == nil {
		return PRPayload{}, fmt.Errorf("parse PR webhook payload: no pull_request field")
	}
	pr, labels, err := body.PullRequest.ToPullRequest("", "")
	if err != nil {
		return PRPayload{}, err
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
	Ref      string // e.g. "refs/heads/main"
	PushedAt string // RFC3339

	// Fields for absorbing the pushed commits into the git-commits cache.
	Before  string // sha of the ref before the push (all-zeros for a new ref)
	After   string // sha of the ref after the push
	Forced  bool
	Commits []PushCommit // pushed commits, payload order (oldest first)
}

// Branch returns the pushed branch name, or "" for a non-branch ref (tags).
func (p PushPayload) Branch() string {
	const prefix = "refs/heads/"
	if strings.HasPrefix(p.Ref, prefix) {
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
