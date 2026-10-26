package webhook

import _ "embed"

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCheckPayload_Status(t *testing.T) {
	raw := json.RawMessage(`{"sha":"s1","state":"success","context":"ci","repository":{"name":"r","owner":{"login":"o"}}}`)
	p, err := ParseCheckPayload("status", raw)
	require.NoError(t, err)
	assert.Equal(t, "o", p.Owner)
	assert.Equal(t, "r", p.Repo)
	assert.Equal(t, "s1", p.SHA)
	assert.Equal(t, "status:ci", p.Context)
	assert.Equal(t, "SUCCESS", p.State)
}

func TestParseCheckPayload_CheckRun(t *testing.T) {
	raw := json.RawMessage(`{"check_run":{"head_sha":"s2","status":"completed","conclusion":"failure","name":"build"},"repository":{"name":"r","owner":{"login":"o"}}}`)
	p, err := ParseCheckPayload("check_run", raw)
	require.NoError(t, err)
	assert.Equal(t, "s2", p.SHA)
	assert.Equal(t, "check_run:build", p.Context)
	assert.Equal(t, "FAILURE", p.State)
}

func TestParseCheckPayload_CheckSuiteInProgress(t *testing.T) {
	raw := json.RawMessage(`{"check_suite":{"head_sha":"s3","status":"in_progress","app":{"slug":"actions"}},"repository":{"name":"r","owner":{"login":"o"}}}`)
	p, err := ParseCheckPayload("check_suite", raw)
	require.NoError(t, err)
	assert.Equal(t, "s3", p.SHA)
	assert.Equal(t, "check_suite:actions", p.Context)
	assert.Equal(t, "PENDING", p.State)
}

func TestParseCheckPayload_Errors(t *testing.T) {
	_, err := ParseCheckPayload("status", json.RawMessage(`not json`))
	assert.Error(t, err)

	_, err = ParseCheckPayload("check_run", json.RawMessage(`{"repository":{"name":"r","owner":{"login":"o"}}}`))
	assert.Error(t, err) // no check_run field

	_, err = ParseCheckPayload("status", json.RawMessage(`{"state":"success"}`))
	assert.Error(t, err) // missing repo/sha

	_, err = ParseCheckPayload("unknown", json.RawMessage(`{}`))
	assert.Error(t, err)
}

func TestParseCheckPayload_OnDefaultBranch(t *testing.T) {
	// check_suite on the default branch.
	p, err := ParseCheckPayload("check_suite", json.RawMessage(`{"check_suite":{"head_sha":"s","head_branch":"main","status":"completed","conclusion":"success"},"repository":{"name":"r","default_branch":"main","owner":{"login":"o"}}}`))
	require.NoError(t, err)
	assert.True(t, p.OnDefaultBranch)

	// check_run on the default branch (branch is nested under check_suite).
	p, err = ParseCheckPayload("check_run", json.RawMessage(`{"check_run":{"head_sha":"s","status":"completed","conclusion":"success","name":"b","check_suite":{"head_branch":"main"}},"repository":{"name":"r","default_branch":"main","owner":{"login":"o"}}}`))
	require.NoError(t, err)
	assert.True(t, p.OnDefaultBranch)

	// status event listing the default branch.
	p, err = ParseCheckPayload("status", json.RawMessage(`{"sha":"s","state":"success","context":"ci","branches":[{"name":"main"}],"repository":{"name":"r","default_branch":"main","owner":{"login":"o"}}}`))
	require.NoError(t, err)
	assert.True(t, p.OnDefaultBranch)

	// not on the default branch.
	p, err = ParseCheckPayload("check_suite", json.RawMessage(`{"check_suite":{"head_sha":"s","head_branch":"feature","status":"completed","conclusion":"success"},"repository":{"name":"r","default_branch":"main","owner":{"login":"o"}}}`))
	require.NoError(t, err)
	assert.False(t, p.OnDefaultBranch)
}

func TestNormalizeStatusState(t *testing.T) {
	assert.Equal(t, "SUCCESS", normalizeStatusState("success"))
	assert.Equal(t, "PENDING", normalizeStatusState("pending"))
	assert.Equal(t, "FAILURE", normalizeStatusState("failure"))
	assert.Equal(t, "ERROR", normalizeStatusState("error"))
	assert.Equal(t, "PENDING", normalizeStatusState("weird"))
}

func TestNormalizeCheckState(t *testing.T) {
	assert.Equal(t, "PENDING", normalizeCheckState("queued", ""))
	assert.Equal(t, "SUCCESS", normalizeCheckState("completed", "neutral"))
	assert.Equal(t, "FAILURE", normalizeCheckState("completed", "timed_out"))
	assert.Equal(t, "PENDING", normalizeCheckState("completed", "weird"))
}

func TestParsePushPayload(t *testing.T) {
	p, err := ParsePushPayload(json.RawMessage(`{"repository":{"name":"r","owner":{"login":"o"}},"head_commit":{"timestamp":"2026-05-01T12:00:00Z"}}`))
	require.NoError(t, err)
	assert.Equal(t, "o", p.Owner)
	assert.Equal(t, "r", p.Repo)
	assert.Equal(t, "2026-05-01T12:00:00Z", p.PushedAt)

	// No head_commit → still produces a non-empty timestamp.
	p, err = ParsePushPayload(json.RawMessage(`{"repository":{"name":"r","owner":{"login":"o"}}}`))
	require.NoError(t, err)
	assert.NotEmpty(t, p.PushedAt)

	_, err = ParsePushPayload(json.RawMessage(`{}`))
	assert.Error(t, err)
}

func TestParseLabelPayload(t *testing.T) {
	p, err := ParseLabelPayload(json.RawMessage(`{"action":"edited","label":{"name":"bug","color":"abc"},"changes":{"name":{"from":"defect"}},"repository":{"name":"r","owner":{"login":"o"}}}`))
	require.NoError(t, err)
	assert.Equal(t, "edited", p.Action)
	assert.Equal(t, "bug", p.Name)
	assert.Equal(t, "abc", p.Color)
	assert.Equal(t, "defect", p.OldName)

	_, err = ParseLabelPayload(json.RawMessage(`{"action":"created"}`))
	assert.Error(t, err) // no label/repository
}

//go:embed testdata/workflow-job-in-progress-json.json
var workflowJobInProgressJSON string

//go:embed testdata/workflow-job-completed-json.json
var workflowJobCompletedJSON string

func TestParseWorkflowJobPayload_InProgress(t *testing.T) {
	p, err := ParseWorkflowJobPayload(json.RawMessage(workflowJobInProgressJSON))
	require.NoError(t, err)
	assert.Equal(t, "o", p.Owner)
	assert.Equal(t, "r", p.Repo)
	assert.Equal(t, int64(987654321), p.JobID)
	assert.Equal(t, int64(123456789), p.RunID)
	assert.Equal(t, int64(2), p.RunAttempt)
	assert.Equal(t, "build (ubuntu-latest)", p.Name)
	assert.Equal(t, "CI", p.WorkflowName)
	assert.Equal(t, "in_progress", p.Status)
	assert.Equal(t, "", p.Conclusion) // null until completed
	assert.Equal(t, "d6fde92930d4715a2b49857d24b940956b26d2d3", p.HeadSHA)
	assert.Equal(t, "main", p.HeadBranch)
	assert.Equal(t, "https://github.com/o/r/actions/runs/123456789/job/987654321", p.HTMLURL)
	assert.Equal(t, "2026-07-03T10:00:00Z", p.StartedAt)
	assert.Equal(t, "", p.CompletedAt) // null until completed
	assert.Equal(t, "GitHub Actions 42", p.RunnerName)
}

func TestParseWorkflowJobPayload_Completed(t *testing.T) {
	p, err := ParseWorkflowJobPayload(json.RawMessage(workflowJobCompletedJSON))
	require.NoError(t, err)
	assert.Equal(t, "completed", p.Status)
	assert.Equal(t, "failure", p.Conclusion)
	assert.Equal(t, "2026-07-03T10:00:00Z", p.StartedAt)
	assert.Equal(t, "2026-07-03T10:05:30Z", p.CompletedAt)
}

func TestParseWorkflowJobPayload_Errors(t *testing.T) {
	// No workflow_job field.
	_, err := ParseWorkflowJobPayload(json.RawMessage(`{"repository":{"name":"r","owner":{"login":"o"}}}`))
	assert.Error(t, err)

	// No repository field.
	_, err = ParseWorkflowJobPayload(json.RawMessage(`{"workflow_job":{"id":1,"status":"completed"}}`))
	assert.Error(t, err)

	// Missing job id.
	_, err = ParseWorkflowJobPayload(json.RawMessage(`{"workflow_job":{"status":"completed"},"repository":{"name":"r","owner":{"login":"o"}}}`))
	assert.Error(t, err)

	// Invalid JSON.
	_, err = ParseWorkflowJobPayload(json.RawMessage(`{nope`))
	assert.Error(t, err)
}
