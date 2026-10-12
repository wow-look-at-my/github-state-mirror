package ghdata

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Storage for GET /repos/{owner}/{repo}/commits/{sha}/branches-where-head on repo_answer_cache.

// BranchesWhereHeadTTL backstops a lost push delivery.
const BranchesWhereHeadTTL = time.Hour

// StoredBranchHead is one trimmed entry: commit.url is dropped.
type StoredBranchHead struct {
	Name      string          `json:"name"`
	Commit    StoredTagCommit `json:"commit"`
	Protected bool            `json:"protected"`
}

// RemoveBranchHead drops branch from the stored answers that list it. A
// sha's answer is only the branches whose tip it is, so a branch that left
// the sha leaves the list. An empty sha means every stored answer, for a
// delete delivery that names the branch and no sha. It reports false when
// an answer cannot be read; the caller then flushes.
func (s *Store) RemoveBranchHead(ctx context.Context, owner, repo, sha, branch string, now time.Time) (bool, error) {
	rows, err := s.ListRepoAnswersOfKind(ctx, owner, repo, RepoAnswerKindBranchesWhereHead, now)
	if err != nil {
		return false, err
	}
	sha = strings.ToLower(sha)
	for _, row := range rows {
		if row.Status != http.StatusOK || (sha != "" && row.Subject != sha) {
			continue
		}
		var items []StoredBranchHead
		if err := json.Unmarshal([]byte(row.Doc), &items); err != nil {
			return false, nil
		}
		kept := make([]StoredBranchHead, 0, len(items))
		for _, item := range items {
			if item.Name != branch {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(items) {
			continue
		}
		doc, err := MarshalCacheDoc(kept)
		if err != nil {
			return false, nil
		}
		row.Doc = string(doc)
		if err := s.PutRepoAnswer(ctx, row.Key, row.RepoAnswer, now); err != nil {
			return false, err
		}
	}
	return true, nil
}
