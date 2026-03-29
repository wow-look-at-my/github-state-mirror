package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestParseEvent_PullRequest(t *testing.T) {
	payload := map[string]interface{}{
		"action": "opened",
		"repository": map[string]interface{}{
			"name": "my-repo",
			"owner": map[string]interface{}{
				"login": "my-org",
			},
		},
		"pull_request": map[string]interface{}{
			"number": 42,
			"base":   map[string]interface{}{"ref": "main"},
			"head":   map[string]interface{}{"ref": "feature"},
		},
		"organization": map[string]interface{}{
			"login": "my-org",
		},
	}
	data, _ := json.Marshal(payload)

	event := ParseEvent("pull_request", data)

	if event.Type != "pull_request" {
		t.Errorf("type = %q, want %q", event.Type, "pull_request")
	}
	if event.Action != "opened" {
		t.Errorf("action = %q, want %q", event.Action, "opened")
	}
	if event.RepoOwner() != "my-org" {
		t.Errorf("repo owner = %q, want %q", event.RepoOwner(), "my-org")
	}
	if event.RepoName() != "my-repo" {
		t.Errorf("repo name = %q, want %q", event.RepoName(), "my-repo")
	}
	if event.PRNumber != 42 {
		t.Errorf("pr number = %d, want %d", event.PRNumber, 42)
	}
	if event.PRBase != "main" {
		t.Errorf("pr base = %q, want %q", event.PRBase, "main")
	}
	if event.PRHead != "feature" {
		t.Errorf("pr head = %q, want %q", event.PRHead, "feature")
	}
	if event.OrgLogin != "my-org" {
		t.Errorf("org login = %q, want %q", event.OrgLogin, "my-org")
	}
	if event.RepoFullName() != "my-org/my-repo" {
		t.Errorf("full name = %q, want %q", event.RepoFullName(), "my-org/my-repo")
	}
}

func TestParseEvent_Push(t *testing.T) {
	payload := map[string]interface{}{
		"ref": "refs/heads/main",
		"repository": map[string]interface{}{
			"name": "my-repo",
			"owner": map[string]interface{}{
				"login": "my-org",
			},
		},
	}
	data, _ := json.Marshal(payload)

	event := ParseEvent("push", data)
	if event.Type != "push" {
		t.Errorf("type = %q, want %q", event.Type, "push")
	}
	if event.RepoOwner() != "my-org" {
		t.Errorf("repo owner = %q, want %q", event.RepoOwner(), "my-org")
	}
	if event.PRNumber != 0 {
		t.Errorf("pr number = %d, want 0", event.PRNumber)
	}
}

func TestParseEvent_InvalidJSON(t *testing.T) {
	event := ParseEvent("push", []byte("not json"))
	if event.Type != "push" {
		t.Errorf("type = %q, want %q", event.Type, "push")
	}
	// Should not panic, fields should be zero.
	if event.RepoOwner() != "" {
		t.Errorf("repo owner should be empty, got %q", event.RepoOwner())
	}
}

func TestVerifySignature(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"action":"opened"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifySignature(secret, sig, body) {
		t.Error("valid signature rejected")
	}
	if verifySignature(secret, "sha256=deadbeef", body) {
		t.Error("invalid signature accepted")
	}
	if verifySignature(secret, "invalid", body) {
		t.Error("malformed signature accepted")
	}
	if verifySignature(secret, "", body) {
		t.Error("empty signature accepted")
	}
}
