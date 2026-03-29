package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"github.com/wow-look-at-my/testify/assert"
)

func TestParseEvent_PullRequest(t *testing.T) {
	payload := map[string]interface{}{
		"action":	"opened",
		"repository": map[string]interface{}{
			"name":	"my-repo",
			"owner": map[string]interface{}{
				"login": "my-org",
			},
		},
		"pull_request": map[string]interface{}{
			"number":	42,
			"base":		map[string]interface{}{"ref": "main"},
			"head":		map[string]interface{}{"ref": "feature"},
		},
		"organization": map[string]interface{}{
			"login": "my-org",
		},
	}
	data, _ := json.Marshal(payload)

	event := ParseEvent("pull_request", data)

	assert.Equal(t, "pull_request", event.Type)

	assert.Equal(t, "opened", event.Action)

	assert.Equal(t, "my-org", event.RepoOwner())

	assert.Equal(t, "my-repo", event.RepoName())

	assert.Equal(t, int64(42), event.PRNumber)

	assert.Equal(t, "main", event.PRBase)

	assert.Equal(t, "feature", event.PRHead)

	assert.Equal(t, "my-org", event.OrgLogin)

	assert.Equal(t, "my-org/my-repo", event.RepoFullName())

}

func TestParseEvent_Push(t *testing.T) {
	payload := map[string]interface{}{
		"ref":	"refs/heads/main",
		"repository": map[string]interface{}{
			"name":	"my-repo",
			"owner": map[string]interface{}{
				"login": "my-org",
			},
		},
	}
	data, _ := json.Marshal(payload)

	event := ParseEvent("push", data)
	assert.Equal(t, "push", event.Type)

	assert.Equal(t, "my-org", event.RepoOwner())

	assert.Equal(t, int64(0), event.PRNumber)

}

func TestParseEvent_InvalidJSON(t *testing.T) {
	event := ParseEvent("push", []byte("not json"))
	assert.Equal(t, "push", event.Type)

	// Should not panic, fields should be zero.
	assert.Equal(t, "", event.RepoOwner())

}

func TestVerifySignature(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"action":"opened"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	assert.True(t, verifySignature(secret, sig, body))

	assert.False(t, verifySignature(secret, "sha256=deadbeef", body))

	assert.False(t, verifySignature(secret, "invalid", body))

	assert.False(t, verifySignature(secret, "", body))

}
