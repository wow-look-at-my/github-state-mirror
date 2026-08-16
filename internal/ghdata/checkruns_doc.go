package ghdata

import (
	"encoding/json"
	"strings"
)

// The stored shape of GET /repos/{owner}/{repo}/commits/{ref}/check-runs
// (commit_ci_cache, kind "check_runs").
//
// It lives here for the same reason the job shapes do: TWO writers render it
// and both must produce the same bytes for the same check run -- the
// fetch-on-miss path, and the `check_run` delivery that rewrites one entry
// inside a stored page (ApplyCheckRunToCommitCI). The TRIM is here too, so
// GitHub's check-run object has exactly one answer for what it becomes,
// whether it arrived in a REST body or in a delivery.
//
// Field order is wire order. What is kept and what is dropped comes from the
// 2026-07-11 consumer survey: required-builds reads output.title and renders
// details_url/html_url, which are pinned exceptions to the no-URL doctrine;
// `url`, node_id/external_id, check_suite, pull_requests and the rest of
// `output` (unbounded display markdown) stay dropped.

// StoredCheckRunApp is a check run's producing app, trimmed to its id -- the
// one app field the known consumer contract branches on.
type StoredCheckRunApp struct {
	ID int64 `json:"id"`
}

// StoredCheckRunOutput is a check run's output, trimmed to its title.
type StoredCheckRunOutput struct {
	Title *string `json:"title"` // nullable; key always emitted
}

// StoredCheckRun is one trimmed check run.
type StoredCheckRun struct {
	ID          int64                `json:"id"`
	HeadSHA     string               `json:"head_sha"`
	Name        string               `json:"name"`
	Status      string               `json:"status"`
	Conclusion  *string              `json:"conclusion"`   // nullable until completed
	StartedAt   *string              `json:"started_at"`   // nullable while queued
	CompletedAt *string              `json:"completed_at"` // nullable until completed
	App         *StoredCheckRunApp   `json:"app"`          // nullable; trimmed to {id}
	Output      StoredCheckRunOutput `json:"output"`       // always emitted; trimmed to {title}
	DetailsURL  *string              `json:"details_url"`  // nullable; pinned consumer-read exception
	HTMLURL     *string              `json:"html_url"`     // nullable; pinned consumer-read exception
}

// StoredCheckRunsPage is one page of a ref's check runs. TotalCount is
// GitHub's TOTAL and can exceed the page's own length.
type StoredCheckRunsPage struct {
	TotalCount int64            `json:"total_count"`
	CheckRuns  []StoredCheckRun `json:"check_runs"`
}

// RawCheckRun mirrors the fields of GitHub's check-run object this route
// models. The same shape arrives in a REST body and inside a `check_run`
// delivery's `check_run` key.
type RawCheckRun struct {
	ID          int64   `json:"id"`
	HeadSHA     string  `json:"head_sha"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
	App         *struct {
		ID int64 `json:"id"`
	} `json:"app"`
	Output *struct {
		Title *string `json:"title"`
	} `json:"output"`
	DetailsURL *string `json:"details_url"`
	HTMLURL    *string `json:"html_url"`
}

// TrimCheckRun converts one raw check run, reporting false when the model
// cannot hold it (no status, or a head sha that is not a full hex object id).
func TrimCheckRun(cr RawCheckRun) (StoredCheckRun, bool) {
	sha := strings.ToLower(cr.HeadSHA)
	if cr.Status == "" || !IsFullHexSHA(sha) {
		return StoredCheckRun{}, false
	}
	out := StoredCheckRun{
		ID: cr.ID, HeadSHA: sha, Name: cr.Name, Status: cr.Status,
		Conclusion: cr.Conclusion, StartedAt: cr.StartedAt, CompletedAt: cr.CompletedAt,
		DetailsURL: cr.DetailsURL, HTMLURL: cr.HTMLURL,
	}
	if cr.App != nil {
		out.App = &StoredCheckRunApp{ID: cr.App.ID}
	}
	// GitHub always sends the output object on real check runs; a missing or
	// null one still rebuilds as {"title": null} so the key is stable.
	if cr.Output != nil {
		out.Output = StoredCheckRunOutput{Title: cr.Output.Title}
	}
	return out, true
}

// TrimCheckRunJSON trims GitHub's check-run object straight from its JSON,
// which is how a `check_run` delivery reaches the rewrite.
func TrimCheckRunJSON(raw json.RawMessage) (StoredCheckRun, bool) {
	var cr RawCheckRun
	if err := json.Unmarshal(raw, &cr); err != nil {
		return StoredCheckRun{}, false
	}
	return TrimCheckRun(cr)
}
