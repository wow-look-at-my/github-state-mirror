package webhook

import (
	"encoding/json"
	"testing"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
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
