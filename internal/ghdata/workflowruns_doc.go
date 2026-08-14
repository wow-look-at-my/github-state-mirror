package ghdata

import (
	"encoding/json"
	"strings"
	"time"
)

// WorkflowRunsCacheTTL bounds how long a stale per-commit runs page can be
// served. It lives here because BOTH writers need it: the fetch-on-miss path
// and the `workflow_run` delivery that rewrites a page. What it backstops is
// the one signal GitHub never webhooks -- run DELETION -- and a missed
// delivery.
const WorkflowRunsCacheTTL = 24 * time.Hour

// The stored shape of GET /repos/{owner}/{repo}/actions/runs?head_sha=...
// (workflow_runs_cache).
//
// Here rather than in the API layer for the same reason as the other rewritten
// documents: two writers render it -- the fetch-on-miss path and the
// `workflow_run` delivery that rewrites one entry inside a stored page
// (ApplyWorkflowRunToPages) -- and a rewritten answer has to be
// indistinguishable from the fetch it saved.
//
// Field order is wire order. Kept and dropped per the 2026-07-11 consumer
// survey: required-builds reads name/status/conclusion/html_url, so html_url
// is a pinned exception to the no-URL doctrine; node_id, the
// head_branch/event/actor/repository/head_commit objects, every other *_url,
// and the unbounded pull_requests/referenced_workflows arrays stay dropped.

// StoredWorkflowRunItem is one trimmed run of a runs listing.
type StoredWorkflowRunItem struct {
	ID           int64   `json:"id"`
	Name         *string `json:"name"` // nullable (a run may have no name)
	HeadSHA      string  `json:"head_sha"`
	Status       string  `json:"status"`
	Conclusion   *string `json:"conclusion"` // nullable until completed
	HTMLURL      string  `json:"html_url"`   // pinned consumer-read exception
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	RunStartedAt *string `json:"run_started_at"` // nullable while queued
}

// StoredWorkflowRunsPage is one page of a runs listing. TotalCount is
// GitHub's TOTAL matching-run count, NOT the page length -- pr-minder's
// zombie probe sends per_page=1 and reads exactly this field.
type StoredWorkflowRunsPage struct {
	TotalCount   int64                   `json:"total_count"`
	WorkflowRuns []StoredWorkflowRunItem `json:"workflow_runs"`
}

// RawWorkflowRun mirrors the fields of GitHub's run object this route models,
// including the two that go to TRUTH but never onto the wire (head_branch,
// run_attempt). The same shape arrives in a REST body and inside a
// `workflow_run` delivery's `workflow_run` key.
type RawWorkflowRun struct {
	ID           int64   `json:"id"`
	RunAttempt   int64   `json:"run_attempt"`
	Name         *string `json:"name"`
	HeadSHA      string  `json:"head_sha"`
	HeadBranch   *string `json:"head_branch"`
	Status       string  `json:"status"`
	Conclusion   *string `json:"conclusion"`
	HTMLURL      string  `json:"html_url"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	RunStartedAt *string `json:"run_started_at"`
}

// TrimWorkflowRunItem converts one raw run, reporting false when the model
// cannot hold it (no id, no status, or a head sha that is not full hex).
func TrimWorkflowRunItem(run RawWorkflowRun) (StoredWorkflowRunItem, bool) {
	sha := strings.ToLower(run.HeadSHA)
	if run.ID <= 0 || run.Status == "" || !IsFullHexSHA(sha) {
		return StoredWorkflowRunItem{}, false
	}
	return StoredWorkflowRunItem{
		ID: run.ID, Name: run.Name, HeadSHA: sha, Status: run.Status,
		Conclusion: run.Conclusion, HTMLURL: run.HTMLURL,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, RunStartedAt: run.RunStartedAt,
	}, true
}

// TrimWorkflowRunItemJSON trims GitHub's run object straight from its JSON,
// which is how a `workflow_run` delivery reaches the rewrite.
func TrimWorkflowRunItemJSON(raw json.RawMessage) (StoredWorkflowRunItem, bool) {
	var run RawWorkflowRun
	if err := json.Unmarshal(raw, &run); err != nil {
		return StoredWorkflowRunItem{}, false
	}
	return TrimWorkflowRunItem(run)
}
