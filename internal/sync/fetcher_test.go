package sync

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestParseOwnerRepoNumber(t *testing.T) {
	tests := []struct {
		key        string
		wantOwner  string
		wantRepo   string
		wantNumber int64
		wantErr    bool
	}{
		{"org/repo/42", "org", "repo", 42, false},
		{"my-org/my-repo/1", "my-org", "my-repo", 1, false},
		{"org/repo", "", "", 0, true},
		{"org/repo/abc", "", "", 0, true},
		{"org", "", "", 0, true},
	}

	for _, tt := range tests {
		owner, repo, number, err := parseOwnerRepoNumber(tt.key)
		assert.Equal(t, tt.wantErr, (err != nil))

		if !tt.wantErr {
			assert.False(t, owner != tt.wantOwner || repo != tt.wantRepo || number != tt.wantNumber)

		}
	}
}

func TestParseCompareKey(t *testing.T) {
	tests := []struct {
		key       string
		wantOwner string
		wantRepo  string
		wantBase  string
		wantHead  string
		wantErr   bool
	}{
		{"org/repo/main...feature", "org", "repo", "main", "feature", false},
		{"my-org/my-repo/develop...hotfix/1", "my-org", "my-repo", "develop", "hotfix/1", false},
		{"org/repo/main", "", "", "", "", true},
		{"org", "", "", "", "", true},
	}

	for _, tt := range tests {
		owner, repo, base, head, err := parseCompareKey(tt.key)
		assert.Equal(t, tt.wantErr, (err != nil))

		if !tt.wantErr {
			assert.False(t, owner != tt.wantOwner || repo != tt.wantRepo || base != tt.wantBase || head != tt.wantHead)

		}
	}
}
