package sync

import (
	"testing"
)

func TestParseOwnerRepoNumber(t *testing.T) {
	tests := []struct {
		key            string
		wantOwner      string
		wantRepo       string
		wantNumber     int64
		wantErr        bool
	}{
		{"org/repo/42", "org", "repo", 42, false},
		{"my-org/my-repo/1", "my-org", "my-repo", 1, false},
		{"org/repo", "", "", 0, true},
		{"org/repo/abc", "", "", 0, true},
		{"org", "", "", 0, true},
	}

	for _, tt := range tests {
		owner, repo, number, err := parseOwnerRepoNumber(tt.key)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseOwnerRepoNumber(%q) err=%v, wantErr=%v", tt.key, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if owner != tt.wantOwner || repo != tt.wantRepo || number != tt.wantNumber {
				t.Errorf("parseOwnerRepoNumber(%q) = (%q, %q, %d), want (%q, %q, %d)",
					tt.key, owner, repo, number, tt.wantOwner, tt.wantRepo, tt.wantNumber)
			}
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
		if (err != nil) != tt.wantErr {
			t.Errorf("parseCompareKey(%q) err=%v, wantErr=%v", tt.key, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if owner != tt.wantOwner || repo != tt.wantRepo || base != tt.wantBase || head != tt.wantHead {
				t.Errorf("parseCompareKey(%q) = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
					tt.key, owner, repo, base, head, tt.wantOwner, tt.wantRepo, tt.wantBase, tt.wantHead)
			}
		}
	}
}
