package webhook

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// What a delivery says about WHEN it happened, and WHAT it is a view of.
//
// GitHub does not order deliveries. It sends each one as it is produced, over
// separate connections, through a tunnel, into a server that may be restarting
// -- so two views of the same resource can arrive in either order, and the
// older one applied second is a correct, repeatable write of superseded state.
// A merged PR came back OPEN 82 seconds after its merge exactly this way.
//
// There is no timestamp HEADER to lean on. GitHub sends `X-GitHub-Delivery`
// (a UUID, not time-ordered), `X-GitHub-Event`, `X-GitHub-Hook-ID` and the
// signature; everything else on the wire here is added by Cloudflare's tunnel
// (`CF-Ray`, `CF-Connecting-IP`, `X-Forwarded-*`) and describes OUR hop, not
// GitHub's. `Date` is likewise the proxy's clock at forward time, which moves
// with the retry, not with the event. So the clock has to come from the
// payload, which is also the only thing the signature covers.
//
// Two rules make a payload field usable as that clock:
//
//   - GitHub must set it, not a user. A push's `head_commit.timestamp` is the
//     COMMIT date: it is whatever the committer's machine claimed, it survives
//     a rebase unchanged, and it can be in the future. `repository.pushed_at`
//     is GitHub's own record of the push and is what this uses.
//   - It must describe the SUBJECT, not the repo around it. `updated_at` on a
//     pull_request is the PR's; `repository.updated_at` riding along in the
//     same payload is not.
//
// A payload with no such field is UNORDERABLE, and says so (ok=false) rather
// than being handed a fabricated time -- a delivery nobody can order must
// still apply, and pretending otherwise would silently drop it.

// EventOrder is one delivery's position: the resource it describes, the moment
// its view is from, and which payload field that moment came from (named for
// the dashboard, so an operator reading an out-of-order report can tell which
// clock produced it).
type EventOrder struct {
	Subject string
	At      time.Time
	Field   string
}

// orderPayload is every timestamp/identity field the extractors below read,
// in one unmarshal. Pointers where absence is meaningful.
type orderPayload struct {
	Action     string `json:"action"`
	Repository *struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		// PUSH payloads send this as a Unix INTEGER while every other event
		// sends an RFC3339 string, so it is decoded by hand. A typed field
		// would fail on one of the two shapes -- and because that failure is
		// the whole struct's, it would take every other field in this payload
		// with it and report a perfectly orderable delivery as unorderable.
		PushedAt  json.RawMessage `json:"pushed_at"`
		UpdatedAt string          `json:"updated_at"`
	} `json:"repository"`
	Ref         string `json:"ref"`
	PullRequest *struct {
		Number    int64  `json:"number"`
		UpdatedAt string `json:"updated_at"`
	} `json:"pull_request"`
	Issue *struct {
		Number int64 `json:"number"`
	} `json:"issue"`
	Comment *struct {
		ID        int64  `json:"id"`
		UpdatedAt string `json:"updated_at"`
	} `json:"comment"`
	Review *struct {
		ID          int64  `json:"id"`
		SubmittedAt string `json:"submitted_at"`
	} `json:"review"`
	// `status` events are flat: id/sha/context/state/updated_at at the root.
	ID        int64  `json:"id"`
	SHA       string `json:"sha"`
	Context   string `json:"context"`
	UpdatedAt string `json:"updated_at"`
	CheckRun  *struct {
		ID          int64  `json:"id"`
		StartedAt   string `json:"started_at"`
		CompletedAt string `json:"completed_at"`
	} `json:"check_run"`
	CheckSuite *struct {
		ID        int64  `json:"id"`
		UpdatedAt string `json:"updated_at"`
	} `json:"check_suite"`
	WorkflowRun *struct {
		ID        int64  `json:"id"`
		UpdatedAt string `json:"updated_at"`
	} `json:"workflow_run"`
	WorkflowJob *struct {
		ID          int64  `json:"id"`
		StartedAt   string `json:"started_at"`
		CompletedAt string `json:"completed_at"`
	} `json:"workflow_job"`
	Installation *struct {
		ID        int64  `json:"id"`
		UpdatedAt string `json:"updated_at"`
	} `json:"installation"`
}

// OrderOf reports the delivery's subject and event time, or ok=false when the
// payload states no time this service can order by. An unorderable delivery is
// applied unconditionally -- refusing one would drop state over a clock we
// never had.
func OrderOf(e Event) (EventOrder, bool) {
	var p orderPayload
	if err := json.Unmarshal(e.Raw, &p); err != nil {
		return EventOrder{}, false
	}
	repo := p.repoKey()

	switch e.Type {
	case "push":
		// The ref is the subject: a push moves exactly one, and two pushes to
		// different branches are unordered with respect to each other.
		if repo == "" || p.Ref == "" {
			return EventOrder{}, false
		}
		at, ok := p.pushedAt()
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: "ref:" + repo + ":" + p.Ref, At: at, Field: "repository.pushed_at"}, true

	case "pull_request":
		if repo == "" || p.PullRequest == nil || p.PullRequest.Number == 0 {
			return EventOrder{}, false
		}
		at, ok := parseEventTime(p.PullRequest.UpdatedAt)
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: fmt.Sprintf("pr:%s#%d", repo, p.PullRequest.Number), At: at, Field: "pull_request.updated_at"}, true

	case "pull_request_review":
		// The REVIEW is the subject, not the PR: two reviews on one PR are
		// independent, and only a re-delivery of the same review is ordered
		// against this one.
		if repo == "" || p.Review == nil || p.Review.ID == 0 {
			return EventOrder{}, false
		}
		at, ok := parseEventTime(p.Review.SubmittedAt)
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: fmt.Sprintf("review:%s:%d", repo, p.Review.ID), At: at, Field: "review.submitted_at"}, true

	case "issue_comment":
		if repo == "" || p.Comment == nil || p.Comment.ID == 0 {
			return EventOrder{}, false
		}
		at, ok := parseEventTime(p.Comment.UpdatedAt)
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: fmt.Sprintf("comment:%s:%d", repo, p.Comment.ID), At: at, Field: "comment.updated_at"}, true

	case "status":
		// sha + CONTEXT, never the sha alone: one commit carries many
		// contexts, and they supersede only themselves. Keying on the sha
		// would let `all-builds` drop a `ci` result for arriving "late".
		if repo == "" || p.SHA == "" || p.Context == "" {
			return EventOrder{}, false
		}
		at, ok := parseEventTime(p.UpdatedAt)
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: "status:" + repo + ":" + p.SHA + ":" + p.Context, At: at, Field: "updated_at"}, true

	case "check_run":
		if repo == "" || p.CheckRun == nil || p.CheckRun.ID == 0 {
			return EventOrder{}, false
		}
		at, field, ok := firstTime([2][2]string{
			{p.CheckRun.CompletedAt, "check_run.completed_at"},
			{p.CheckRun.StartedAt, "check_run.started_at"},
		})
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: fmt.Sprintf("check_run:%s:%d", repo, p.CheckRun.ID), At: at, Field: field}, true

	case "check_suite":
		if repo == "" || p.CheckSuite == nil || p.CheckSuite.ID == 0 {
			return EventOrder{}, false
		}
		at, ok := parseEventTime(p.CheckSuite.UpdatedAt)
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: fmt.Sprintf("check_suite:%s:%d", repo, p.CheckSuite.ID), At: at, Field: "check_suite.updated_at"}, true

	case "workflow_run":
		if repo == "" || p.WorkflowRun == nil || p.WorkflowRun.ID == 0 {
			return EventOrder{}, false
		}
		at, ok := parseEventTime(p.WorkflowRun.UpdatedAt)
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: fmt.Sprintf("workflow_run:%s:%d", repo, p.WorkflowRun.ID), At: at, Field: "workflow_run.updated_at"}, true

	case "workflow_job":
		if repo == "" || p.WorkflowJob == nil || p.WorkflowJob.ID == 0 {
			return EventOrder{}, false
		}
		at, field, ok := firstTime([2][2]string{
			{p.WorkflowJob.CompletedAt, "workflow_job.completed_at"},
			{p.WorkflowJob.StartedAt, "workflow_job.started_at"},
		})
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: fmt.Sprintf("workflow_job:%s:%d", repo, p.WorkflowJob.ID), At: at, Field: field}, true

	case "repository":
		if repo == "" || p.Repository == nil {
			return EventOrder{}, false
		}
		at, ok := parseEventTime(p.Repository.UpdatedAt)
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: "repo:" + repo, At: at, Field: "repository.updated_at"}, true

	case "installation", "installation_repositories":
		if p.Installation == nil || p.Installation.ID == 0 {
			return EventOrder{}, false
		}
		at, ok := parseEventTime(p.Installation.UpdatedAt)
		if !ok {
			return EventOrder{}, false
		}
		return EventOrder{Subject: fmt.Sprintf("installation:%d", p.Installation.ID), At: at, Field: "installation.updated_at"}, true
	}

	// Everything else -- `label` (no timestamp anywhere in the payload),
	// `organization`, `membership` -- is unorderable and applies as it always
	// did. Reported, never guessed at.
	return EventOrder{}, false
}

func (p orderPayload) repoKey() string {
	if p.Repository == nil {
		return ""
	}
	if p.Repository.FullName != "" {
		return strings.ToLower(p.Repository.FullName)
	}
	if p.Repository.Owner.Login == "" || p.Repository.Name == "" {
		return ""
	}
	return strings.ToLower(p.Repository.Owner.Login + "/" + p.Repository.Name)
}

// pushedAt reads repository.pushed_at, which push payloads send as a Unix
// integer and every other event as an RFC3339 string.
func (p orderPayload) pushedAt() (time.Time, bool) {
	if p.Repository == nil {
		return time.Time{}, false
	}
	raw := strings.TrimSpace(string(p.Repository.PushedAt))
	if raw == "" || raw == "null" {
		return time.Time{}, false
	}
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return time.Time{}, false
		}
		return parseEventTime(s)
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).UTC(), true
}

// firstTime picks the first parseable timestamp from a preference list, and
// names which one it used.
func firstTime(candidates [2][2]string) (time.Time, string, bool) {
	for _, c := range candidates {
		if at, ok := parseEventTime(c[0]); ok {
			return at, c[1], true
		}
	}
	return time.Time{}, "", false
}

func parseEventTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil || at.IsZero() {
		return time.Time{}, false
	}
	return at.UTC(), true
}
