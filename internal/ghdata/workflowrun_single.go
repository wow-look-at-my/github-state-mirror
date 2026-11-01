// The single-run read, GET /repos/{owner}/{repo}/actions/runs/{run_id}, is rebuilt from the workflow_runs truth row.

package ghdata

import _ "embed"

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// The single-run read, GET /repos/{owner}/{repo}/actions/runs/{run_id}, is
// rebuilt from the workflow_runs truth row.

const (
	// WorkflowRunSettledTTL bounds how long a completed run serves without a fresh whole object.
	WorkflowRunSettledTTL = 24 * time.Hour

	// WorkflowRunLiveTTL bounds an unfinished run. Every run transition is a delivery, so this only covers a lost one.
	WorkflowRunLiveTTL = time.Minute
)

// RunUser is the trimmed account object on a run.
type RunUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

// RunPerson is a commit author or committer.
type RunPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// RunHeadCommit is the trimmed head_commit object.
type RunHeadCommit struct {
	ID        string     `json:"id"`
	TreeID    string     `json:"tree_id"`
	Message   string     `json:"message"`
	Timestamp string     `json:"timestamp"`
	Author    *RunPerson `json:"author"`
	Committer *RunPerson `json:"committer"`
}

// RunRepository is the trimmed repository and head_repository object.
type RunRepository struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	FullName string   `json:"full_name"`
	Private  bool     `json:"private"`
	Owner    *RunUser `json:"owner"`
}

// RunPullRequestRepo is the repo of one side of a run's pull request.
type RunPullRequestRepo struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// RunPullRequestSide is the head or base of a run's pull request.
type RunPullRequestSide struct {
	Ref  string             `json:"ref"`
	SHA  string             `json:"sha"`
	Repo RunPullRequestRepo `json:"repo"`
}

// RunPullRequest is one entry of a run's pull_requests.
type RunPullRequest struct {
	ID     int64              `json:"id"`
	Number int64              `json:"number"`
	Head   RunPullRequestSide `json:"head"`
	Base   RunPullRequestSide `json:"base"`
}

// WorkflowRunDetail is the part of the single-run read the truth columns
// do not hold. It is stored as JSON in workflow_runs.detail.
type WorkflowRunDetail struct {
	NodeID          string           `json:"node_id"`
	DisplayTitle    string           `json:"display_title"`
	Event           string           `json:"event"`
	Path            string           `json:"path"`
	RunNumber       int64            `json:"run_number"`
	WorkflowID      int64            `json:"workflow_id"`
	CheckSuiteID    int64            `json:"check_suite_id"`
	JobsURL         string           `json:"jobs_url"`
	Actor           *RunUser         `json:"actor"`
	TriggeringActor *RunUser         `json:"triggering_actor"`
	HeadCommit      *RunHeadCommit   `json:"head_commit"`
	Repository      *RunRepository   `json:"repository"`
	HeadRepository  *RunRepository   `json:"head_repository"`
	PullRequests    []RunPullRequest `json:"pull_requests"`
}

// StoredWorkflowRun is the trimmed single-run document. Field order is wire
// order. html_url and jobs_url are pinned consumer-read exceptions: the gh
// CLI links html_url and follows jobs_url to list the run's jobs.
type StoredWorkflowRun struct {
	ID              int64            `json:"id"`
	Name            *string          `json:"name"`
	NodeID          string           `json:"node_id"`
	HeadBranch      *string          `json:"head_branch"`
	HeadSHA         string           `json:"head_sha"`
	Path            string           `json:"path"`
	DisplayTitle    string           `json:"display_title"`
	RunNumber       int64            `json:"run_number"`
	Event           string           `json:"event"`
	Status          string           `json:"status"`
	Conclusion      *string          `json:"conclusion"`
	WorkflowID      int64            `json:"workflow_id"`
	CheckSuiteID    int64            `json:"check_suite_id"`
	HTMLURL         string           `json:"html_url"`
	PullRequests    []RunPullRequest `json:"pull_requests"`
	CreatedAt       string           `json:"created_at"`
	UpdatedAt       string           `json:"updated_at"`
	Actor           *RunUser         `json:"actor"`
	RunAttempt      int64            `json:"run_attempt"`
	RunStartedAt    *string          `json:"run_started_at"`
	TriggeringActor *RunUser         `json:"triggering_actor"`
	JobsURL         string           `json:"jobs_url"`
	HeadCommit      *RunHeadCommit   `json:"head_commit"`
	Repository      *RunRepository   `json:"repository"`
	HeadRepository  *RunRepository   `json:"head_repository"`
}

// rawWorkflowRunObject is GitHub's whole run object, as a REST body and as
// a workflow_run delivery's workflow_run key carry it.
type rawWorkflowRunObject struct {
	RawWorkflowRun
	WorkflowRunDetail
}

// ParseWorkflowRunObject reads a whole run object into a truth row with its
// detail. It reports false when the model cannot hold the run: no id, no
// status, or a head sha that is not full hex.
func ParseWorkflowRunObject(owner, repo string, raw []byte) (WorkflowRun, bool) {
	var obj rawWorkflowRunObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return WorkflowRun{}, false
	}
	item, ok := TrimWorkflowRunItem(obj.RawWorkflowRun)
	if !ok {
		return WorkflowRun{}, false
	}
	if obj.PullRequests == nil {
		obj.PullRequests = []RunPullRequest{}
	}
	detail, err := MarshalCacheDoc(obj.WorkflowRunDetail)
	if err != nil {
		return WorkflowRun{}, false
	}
	return WorkflowRun{
		Owner: owner, Repo: repo, RunID: item.ID, RunAttempt: obj.RunAttempt,
		Name: strOrEmpty(obj.Name), HeadSHA: item.HeadSHA, HeadBranch: strOrEmpty(obj.HeadBranch),
		Status: obj.Status, Conclusion: strOrEmpty(obj.Conclusion), HTMLURL: obj.HTMLURL,
		CreatedAt: obj.CreatedAt, UpdatedAt: obj.UpdatedAt, RunStartedAt: strOrEmpty(obj.RunStartedAt),
		Detail: string(detail),
	}, true
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func emptyAsNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// RenderWorkflowRun rebuilds the single-run document from a truth row. It
// reports false when the row holds no detail: no whole object landed yet.
// excludePRs renders pull_requests empty, as exclude_pull_requests=true does.
func RenderWorkflowRun(r WorkflowRun, excludePRs bool) ([]byte, bool) {
	if r.Detail == "" {
		return nil, false
	}
	var d WorkflowRunDetail
	if err := json.Unmarshal([]byte(r.Detail), &d); err != nil {
		return nil, false
	}
	prs := d.PullRequests
	if excludePRs || prs == nil {
		prs = []RunPullRequest{}
	}
	doc, err := MarshalCacheDoc(StoredWorkflowRun{
		ID: r.RunID, Name: emptyAsNull(r.Name), NodeID: d.NodeID,
		HeadBranch: emptyAsNull(r.HeadBranch), HeadSHA: r.HeadSHA,
		Path: d.Path, DisplayTitle: d.DisplayTitle, RunNumber: d.RunNumber, Event: d.Event,
		Status: r.Status, Conclusion: emptyAsNull(r.Conclusion),
		WorkflowID: d.WorkflowID, CheckSuiteID: d.CheckSuiteID, HTMLURL: r.HTMLURL,
		PullRequests: prs, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		Actor: d.Actor, RunAttempt: r.RunAttempt, RunStartedAt: emptyAsNull(r.RunStartedAt),
		TriggeringActor: d.TriggeringActor, JobsURL: d.JobsURL, HeadCommit: d.HeadCommit,
		Repository: d.Repository, HeadRepository: d.HeadRepository,
	})
	if err != nil {
		return nil, false
	}
	return doc, true
}

// WorkflowRunServable reports whether the single-run read may serve the row
// without asking GitHub: it holds a whole object recent enough for its
// state. A job delivery moves touched_at but proves nothing about the run as
// a whole, so the bound reads detail_at.
func WorkflowRunServable(r WorkflowRun, now time.Time) bool {
	if r.Detail == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, r.DetailAt)
	if err != nil {
		return false
	}
	ttl := WorkflowRunLiveTTL
	if r.Status == "completed" {
		ttl = WorkflowRunSettledTTL
	}
	return now.Before(at.Add(ttl))
}

// dbx returns the plain-SQL handle bound to the transaction ctx carries, or
// the pool. It serves the tables in schema_plain.sql.
func (s *Store) dbx(ctx context.Context) dbgen.DBTX {
	if tx := database.TxFrom(ctx, s.db); tx != nil {
		return tx
	}
	return s.db
}

// applyWorkflowRunDetail records a run's detail when the source carried it.
// The run's own updated_at orders it, so an older view never lands.
func (s *Store) applyWorkflowRunDetail(ctx context.Context, r WorkflowRun, now time.Time) error {
	if r.Detail == "" {
		return nil
	}
	db := s.dbx(ctx)
	if _, err := db.ExecContext(ctx, applyWorkflowRunDetailSql,
		NormalizeRepoKey(r.Owner), NormalizeRepoKey(r.Repo), r.RunID, r.UpdatedAt, r.Detail, rfc3339(now),
	); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM workflow_run_details WHERE detail_at < ?`,
		rfc3339(now.Add(-workflowRunRetention)))
	return err
}

// GetWorkflowRun reads one run's truth row with its detail.
func (s *Store) GetWorkflowRun(ctx context.Context, owner, repo string, runID int64) (WorkflowRun, bool, error) {
	var r WorkflowRun
	var detail, detailAt sql.NullString
	err := s.dbx(ctx).QueryRowContext(ctx, getWorkflowRunSql,
		NormalizeRepoKey(owner), NormalizeRepoKey(repo), runID,
	).Scan(&r.Owner, &r.Repo, &r.RunID, &r.RunAttempt, &r.Name, &r.HeadSHA, &r.HeadBranch,
		&r.Status, &r.Conclusion, &r.HTMLURL, &r.CreatedAt, &r.UpdatedAt, &r.RunStartedAt,
		&detail, &detailAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowRun{}, false, nil
	}
	if err != nil {
		return WorkflowRun{}, false, err
	}
	r.HeadSHA = strings.ToLower(r.HeadSHA)
	r.Detail, r.DetailAt = detail.String, detailAt.String
	return r, true, nil
}

//go:embed apply-workflow-run-detail.sql
var applyWorkflowRunDetailSql string

//go:embed get-workflow-run.sql
var getWorkflowRunSql string
