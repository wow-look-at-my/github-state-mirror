package notify

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeriveDBPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"github-mirror.db", "github-mirror-subscriptions.db"},
		{"/var/data/github-mirror.db", "/var/data/github-mirror-subscriptions.db"},
		{"mirror", "mirror-subscriptions.db"}, // no .db suffix: just append
		{"a.db.db", "a.db-subscriptions.db"},  // only the trailing .db is stripped
	} {
		assert.Equal(t, tc.want, DeriveDBPath(tc.in), "DeriveDBPath(%q)", tc.in)
	}
}

func TestMatchesEvent(t *testing.T) {
	all := Subscription{}
	assert.True(t, all.MatchesEvent("push"), "empty filter admits everything")
	assert.True(t, all.MatchesEvent("pull_request"))

	some := Subscription{EventFilters: []string{"push", "pull_request"}}
	assert.True(t, some.MatchesEvent("push"))
	assert.True(t, some.MatchesEvent("pull_request"))
	assert.False(t, some.MatchesEvent("status"))
	assert.False(t, some.MatchesEvent(""))
}

func TestMatchesRepo(t *testing.T) {
	all := Subscription{}
	assert.True(t, all.MatchesRepo("my-org", "repo1"), "empty filter admits everything")

	byOwner := Subscription{RepoFilters: []string{"my-org"}}
	assert.True(t, byOwner.MatchesRepo("my-org", "anything"), "owner filter admits every repo of the owner")
	assert.True(t, byOwner.MatchesRepo("My-Org", "AnyThing"), "matching is case-insensitive")
	assert.False(t, byOwner.MatchesRepo("other-org", "repo1"))
	assert.False(t, byOwner.MatchesRepo("my-org-2", "repo1"), "an owner filter must not prefix-match")

	byRepo := Subscription{RepoFilters: []string{"my-org/repo1"}}
	assert.True(t, byRepo.MatchesRepo("my-org", "repo1"))
	assert.True(t, byRepo.MatchesRepo("MY-ORG", "REPO1"), "matching is case-insensitive")
	assert.False(t, byRepo.MatchesRepo("my-org", "repo2"))
	assert.False(t, byRepo.MatchesRepo("other", "repo1"))

	both := Subscription{RepoFilters: []string{"aaa", "bbb/ccc"}}
	assert.True(t, both.MatchesRepo("aaa", "x"))
	assert.True(t, both.MatchesRepo("bbb", "ccc"))
	assert.False(t, both.MatchesRepo("bbb", "x"))
}

// validSub returns a subscription that passes validation, for the table test
// to perturb.
func validSub() Subscription {
	return Subscription{
		URL:    "https://example.com/hook",
		Secret: strings.Repeat("s", 32),
	}
}

func TestNormalizeAndValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Subscription)
		wantErr string // "" = valid
	}{
		{"valid https", func(s *Subscription) {}, ""},
		{"valid localhost http", func(s *Subscription) { s.URL = "http://localhost:8080/hook" }, ""},
		{"valid 127.0.0.1 http", func(s *Subscription) { s.URL = "http://127.0.0.1:9999/x" }, ""},
		{"valid 127.5.5.5 http", func(s *Subscription) { s.URL = "http://127.5.5.5/x" }, ""},
		{"valid ::1 http", func(s *Subscription) { s.URL = "http://[::1]:8080/x" }, ""},
		{"valid filters", func(s *Subscription) {
			s.RepoFilters = []string{"My-Org", "My-Org/Repo.One"}
			s.EventFilters = []string{"push", "pull_request"}
		}, ""},

		{"empty url", func(s *Subscription) { s.URL = "" }, "url is required"},
		{"relative url", func(s *Subscription) { s.URL = "/hook" }, "absolute"},
		{"http non-loopback", func(s *Subscription) { s.URL = "http://example.com/hook" }, "loopback"},
		{"bad scheme", func(s *Subscription) { s.URL = "ftp://example.com/x" }, "scheme"},
		{"userinfo", func(s *Subscription) { s.URL = "https://user:pw@example.com/x" }, "userinfo"},
		{"no host", func(s *Subscription) { s.URL = "https:///path" }, "host"},
		{"private 10/8", func(s *Subscription) { s.URL = "https://10.1.2.3/x" }, "private"},
		{"private 172.16/12", func(s *Subscription) { s.URL = "https://172.16.0.1/x" }, "private"},
		{"private 192.168/16", func(s *Subscription) { s.URL = "https://192.168.1.1/x" }, "private"},
		{"link-local 169.254/16", func(s *Subscription) { s.URL = "https://169.254.1.1/x" }, "private"},
		{"ipv6 ULA fc00::/7", func(s *Subscription) { s.URL = "https://[fd00::1]/x" }, "private"},
		{"ipv6 link-local fe80::/10", func(s *Subscription) { s.URL = "https://[fe80::1]/x" }, "private"},
		{"unspecified 0.0.0.0", func(s *Subscription) { s.URL = "https://0.0.0.0/x" }, "private"},
		{"public IP literal ok", func(s *Subscription) { s.URL = "https://93.184.216.34/x" }, ""},

		{"secret too short", func(s *Subscription) { s.Secret = strings.Repeat("s", 15) }, "secret"},
		{"secret too long", func(s *Subscription) { s.Secret = strings.Repeat("s", 257) }, "secret"},
		{"secret min ok", func(s *Subscription) { s.Secret = strings.Repeat("s", 16) }, ""},
		{"secret max ok", func(s *Subscription) { s.Secret = strings.Repeat("s", 256) }, ""},

		{"too many repos", func(s *Subscription) {
			for i := 0; i <= maxFilters; i++ {
				s.RepoFilters = append(s.RepoFilters, "org")
			}
		}, "at most"},
		{"bad repo filter chars", func(s *Subscription) { s.RepoFilters = []string{"bad owner!"} }, "repos[0]"},
		{"repo filter extra slash", func(s *Subscription) { s.RepoFilters = []string{"a/b/c"} }, "repos[0]"},
		{"repo filter empty", func(s *Subscription) { s.RepoFilters = []string{""} }, "repos[0]"},
		{"too many events", func(s *Subscription) {
			for i := 0; i <= maxFilters; i++ {
				s.EventFilters = append(s.EventFilters, "push")
			}
		}, "at most"},
		{"bad event chars", func(s *Subscription) { s.EventFilters = []string{"Push"} }, "events[0]"},
		{"empty event", func(s *Subscription) { s.EventFilters = []string{""} }, "events[0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub := validSub()
			tc.mutate(&sub)
			err := sub.NormalizeAndValidate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			var ve *ValidationError
			assert.ErrorAs(t, err, &ve, "validation failures must be *ValidationError")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNormalizeAndValidate_LowercasesRepoFilters(t *testing.T) {
	sub := validSub()
	sub.RepoFilters = []string{"My-Org", "My-Org/RePo"}
	require.NoError(t, sub.NormalizeAndValidate())
	assert.Equal(t, []string{"my-org", "my-org/repo"}, sub.RepoFilters)
}
