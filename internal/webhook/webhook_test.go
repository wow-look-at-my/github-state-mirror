package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"testing"
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

func TestParsePRPayload_Open(t *testing.T) {
	payload := map[string]interface{}{
		"action":	"opened",
		"pull_request": map[string]interface{}{
			"number":	42,
			"title":	"Add feature",
			"html_url":	"https://github.com/my-org/my-repo/pull/42",
			"draft":	false,
			"state":	"open",
			"created_at":	"2026-04-01T10:00:00Z",
			"updated_at":	"2026-04-01T10:00:00Z",
			"additions":	10,
			"deletions":	3,
			"mergeable":	true,
			"user": map[string]interface{}{
				"login":	"alice",
				"avatar_url":	"https://avatars.githubusercontent.com/alice",
				"html_url":	"https://github.com/alice",
			},
			"head": map[string]interface{}{
				"ref":	"feature",
				"sha":	"abc123",
			},
			"base": map[string]interface{}{
				"ref":	"main",
				"repo": map[string]interface{}{
					"name":	"my-repo",
					"owner": map[string]interface{}{
						"login": "my-org",
					},
				},
			},
			"labels": []map[string]interface{}{
				{"name": "bug", "color": "d73a4a"},
			},
			"requested_reviewers": []map[string]interface{}{
				{"login": "bob"},
			},
			"requested_teams":	[]interface{}{},
		},
	}
	data, _ := json.Marshal(payload)

	result, err := ParsePRPayload(data)
	assert.Nil(t, err)

	pr := result.PR
	assert.Equal(t, "my-org", pr.Owner)
	assert.Equal(t, "my-repo", pr.Repo)
	assert.Equal(t, int64(42), pr.Number)
	assert.Equal(t, "Add feature", pr.Title)
	assert.Equal(t, "OPEN", pr.State)
	assert.Equal(t, int64(0), pr.IsDraft)
	assert.Equal(t, int64(10), pr.Additions.Int64)
	assert.True(t, pr.Additions.Valid)
	assert.Equal(t, int64(3), pr.Deletions.Int64)
	assert.Equal(t, "MERGEABLE", pr.Mergeable.String)
	assert.Equal(t, "alice", pr.AuthorLogin.String)
	assert.Equal(t, "feature", pr.HeadRefName.String)
	assert.Equal(t, "main", pr.BaseRefName.String)
	assert.Equal(t, "abc123", pr.HeadRefOid.String)
	assert.Equal(t, int64(1), pr.ReviewRequestCount.Int64)

	assert.Equal(t, 1, len(result.Labels))
	assert.Equal(t, "bug", result.Labels[0].Name)
	assert.Equal(t, "d73a4a", result.Labels[0].Color)
}

func TestParsePRPayload_Closed(t *testing.T) {
	payload := map[string]interface{}{
		"action":	"closed",
		"pull_request": map[string]interface{}{
			"number":	7,
			"title":	"Fix bug",
			"html_url":	"https://github.com/my-org/my-repo/pull/7",
			"draft":	false,
			"state":	"closed",
			"created_at":	"2026-03-01T10:00:00Z",
			"updated_at":	"2026-04-01T12:00:00Z",
			"user":		map[string]interface{}{"login": "bob", "avatar_url": "", "html_url": ""},
			"head":		map[string]interface{}{"ref": "fix", "sha": "def456"},
			"base": map[string]interface{}{
				"ref":	"main",
				"repo": map[string]interface{}{
					"name":		"my-repo",
					"owner":	map[string]interface{}{"login": "my-org"},
				},
			},
			"labels":		[]interface{}{},
			"requested_reviewers":	[]interface{}{},
			"requested_teams":	[]interface{}{},
		},
	}
	data, _ := json.Marshal(payload)

	result, err := ParsePRPayload(data)
	assert.Nil(t, err)
	assert.Equal(t, "CLOSED", result.PR.State)
	assert.Equal(t, 0, len(result.Labels))
}

func TestParsePRPayload_InvalidJSON(t *testing.T) {
	_, err := ParsePRPayload([]byte("not json"))
	assert.NotNil(t, err)
}

func TestParsePRPayload_NoPullRequest(t *testing.T) {
	_, err := ParsePRPayload([]byte(`{"action":"opened"}`))
	assert.NotNil(t, err)
}

func TestParsePRPayload_Draft(t *testing.T) {
	payload := map[string]interface{}{
		"action":	"opened",
		"pull_request": map[string]interface{}{
			"number":	1,
			"title":	"WIP",
			"html_url":	"https://github.com/o/r/pull/1",
			"draft":	true,
			"state":	"open",
			"created_at":	"2026-04-01T10:00:00Z",
			"updated_at":	"2026-04-01T10:00:00Z",
			"user":		map[string]interface{}{"login": "alice", "avatar_url": "", "html_url": ""},
			"head":		map[string]interface{}{"ref": "wip", "sha": "aaa"},
			"base": map[string]interface{}{
				"ref":	"main",
				"repo": map[string]interface{}{
					"name":		"r",
					"owner":	map[string]interface{}{"login": "o"},
				},
			},
			"labels":		[]interface{}{},
			"requested_reviewers":	[]interface{}{},
			"requested_teams":	[]interface{}{},
		},
	}
	data, _ := json.Marshal(payload)

	result, err := ParsePRPayload(data)
	assert.Nil(t, err)
	assert.Equal(t, int64(1), result.PR.IsDraft)
}

func TestParsePRPayload_MergeableNull(t *testing.T) {
	payload := map[string]interface{}{
		"action":	"opened",
		"pull_request": map[string]interface{}{
			"number":	1,
			"title":	"Test",
			"html_url":	"https://github.com/o/r/pull/1",
			"draft":	false,
			"state":	"open",
			"created_at":	"2026-04-01T10:00:00Z",
			"updated_at":	"2026-04-01T10:00:00Z",
			"mergeable":	nil,
			"user":		map[string]interface{}{"login": "alice", "avatar_url": "", "html_url": ""},
			"head":		map[string]interface{}{"ref": "f", "sha": "aaa"},
			"base": map[string]interface{}{
				"ref":	"main",
				"repo": map[string]interface{}{
					"name":		"r",
					"owner":	map[string]interface{}{"login": "o"},
				},
			},
			"labels":		[]interface{}{},
			"requested_reviewers":	[]interface{}{},
			"requested_teams":	[]interface{}{},
		},
	}
	data, _ := json.Marshal(payload)

	result, err := ParsePRPayload(data)
	assert.Nil(t, err)
	assert.False(t, result.PR.Mergeable.Valid)
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
