package ghdata

import (
	"encoding/json"
	"strings"
	"time"
)

// WorkflowRunsCacheTTL backstops run DELETION, the signal GitHub never webhooks, and a missed delivery.
const WorkflowRunsCacheTTL = 24 * time.Hour

// The stored, trimmed shape of GET /repos/{owner}/{repo}/actions/runs?head_sha=... (workflow_runs_cache).
// Shared by the fetch-on-miss path and the workflow_run delivery rewrite, so a rewritten answer stays
// indistinguishable from a fresh fetch. Field order is wire order. see docs/cache/rest-routes.md

// StoredWorkflowRunItem is trimmed run of a runs listing.
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

// StoredWorkflowRunsPage is page; TotalCount is GitHub's total matching-run count, not the page length.
type StoredWorkflowRunsPage struct {
	TotalCount   int64                   `json:"total_count"`
	WorkflowRuns []StoredWorkflowRunItem `json:"workflow_runs"`
}

// RawWorkflowRun mirrors the fields of GitHub's run object this route models,
// including the that go to TRUTH but never onto the wire (head_branch,
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

// TrimWorkflowRunItem converts raw run, reporting false when the model
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
