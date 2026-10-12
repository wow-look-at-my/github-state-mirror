package ghdata

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Storage for GET /repos/{owner}/{repo}/tags pages on repo_answer_cache.

// TagsCacheTTL backstops a lost tag push.
const TagsCacheTTL = 24 * time.Hour

// StoredTagCommit is a tag's commit, trimmed to its sha.
type StoredTagCommit struct {
	SHA string `json:"sha"`
}

// StoredTag is one trimmed entry of a tags page. zipball_url, tarball_url and
// commit.url are dropped.
type StoredTag struct {
	Name   string          `json:"name"`
	Commit StoredTagCommit `json:"commit"`
	NodeID string          `json:"node_id"`
}

// ApplyPushedTagTip rewrites a moved tag's commit in every stored page that
// lists it. A tip move changes no page's membership or order, so a page
// that does not list the tag is unaffected. It reports false when a page
// cannot be read; the caller then drops the repo's pages.
func (s *Store) ApplyPushedTagTip(ctx context.Context, owner, repo, tag, afterSHA string, now time.Time) (bool, error) {
	afterSHA = strings.ToLower(afterSHA)
	if tag == "" || !IsFullHexSHA(afterSHA) || strings.Trim(afterSHA, "0") == "" {
		return false, nil
	}
	rows, err := s.ListRepoAnswersOfKind(ctx, owner, repo, RepoAnswerKindTags, now)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Status != http.StatusOK {
			continue
		}
		var items []StoredTag
		if err := json.Unmarshal([]byte(row.Doc), &items); err != nil {
			return false, nil
		}
		found := false
		for i := range items {
			if items[i].Name == tag {
				items[i].Commit.SHA = afterSHA
				found = true
			}
		}
		if !found {
			continue
		}
		patched, err := MarshalCacheDoc(items)
		if err != nil {
			return false, nil
		}
		row.Doc = string(patched)
		row.ExpiresAt = now.Add(TagsCacheTTL)
		if err := s.PutRepoAnswer(ctx, row.Key, row.RepoAnswer, now); err != nil {
			return false, err
		}
	}
	return true, nil
}
