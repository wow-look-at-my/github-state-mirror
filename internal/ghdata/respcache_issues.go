// IssueCacheTTL backstops a lost issues or issue_comment delivery.

package ghdata

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// The issue reads on repo_answer_cache: GET /repos/{owner}/{repo}/issues/{number}
// and GET /repos/{owner}/{repo}/issues. see docs/cache/rest-routes.md

const (
	RepoAnswerKindIssue      = "issue"
	RepoAnswerKindIssuesList = "issues_list"

	// IssueCacheTTL backstops a lost issues or issue_comment delivery.
	IssueCacheTTL = 24 * time.Hour

	// IssuesListCacheTTL backstops a lost delivery. Every delivery flushes the listing.
	IssuesListCacheTTL = 10 * time.Minute
)

// isURLKey is the key set the rebuilt documents never carry.
func isURLKey(k string) bool {
	lk := strings.ToLower(k)
	return lk == "url" || lk == "_links" || strings.HasSuffix(lk, "_url")
}

// stripURLKeys removes every URL key from a decoded JSON value, at any depth.
func stripURLKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if isURLKey(k) {
				delete(x, k)
				continue
			}
			x[k] = stripURLKeys(val)
		}
	case []any:
		for i, val := range x {
			x[i] = stripURLKeys(val)
		}
	}
	return v
}

func decodeJSON(raw []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// issueObject decodes an issue and reports its number. It must carry a
// positive number and an updated_at.
func issueObject(raw []byte) (map[string]any, int64, bool) {
	v, ok := decodeJSON(raw)
	if !ok {
		return nil, 0, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, 0, false
	}
	num, ok := m["number"].(json.Number)
	if !ok {
		return nil, 0, false
	}
	n, err := num.Int64()
	if err != nil || n <= 0 {
		return nil, 0, false
	}
	if s, _ := m["updated_at"].(string); s == "" {
		return nil, 0, false
	}
	return m, n, true
}

// RenderIssueDoc renders one issue object as the trimmed document, and reports
// its number. A fetch and a delivery both render through it.
func RenderIssueDoc(raw []byte) (string, int64, bool) {
	m, n, ok := issueObject(raw)
	if !ok {
		return "", 0, false
	}
	doc, err := MarshalCacheDoc(stripURLKeys(m))
	if err != nil {
		return "", 0, false
	}
	return string(doc), n, true
}

// RenderIssuesListDoc renders an issues listing page as the trimmed document.
// Every entry must be an issue object.
func RenderIssuesListDoc(raw []byte) (string, bool) {
	v, ok := decodeJSON(raw)
	if !ok {
		return "", false
	}
	list, ok := v.([]any)
	if !ok {
		return "", false
	}
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return "", false
		}
		if num, ok := m["number"].(json.Number); !ok || num.String() == "" {
			return "", false
		}
	}
	doc, err := MarshalCacheDoc(stripURLKeys(list))
	if err != nil {
		return "", false
	}
	return string(doc), true
}

// IssueAnswerKey addresses the single-issue row.
func IssueAnswerKey(owner, repo string, number int64) RepoAnswerKey {
	return RepoAnswerKey{Owner: owner, Repo: repo, Kind: RepoAnswerKindIssue, RefID: strconv.FormatInt(number, 10)}
}

// ApplyIssuePayload rewrites a stored issue row from a delivery's issue
// object, and reports what it did. The stored fields the payload omits are
// kept. An older view is refused.
func (s *Store) ApplyIssuePayload(ctx context.Context, owner, repo string, raw []byte, now time.Time) (string, error) {
	payload, n, ok := issueObject(raw)
	if !ok {
		return "unparseable issue object", nil
	}
	key := IssueAnswerKey(owner, repo, n)
	stored, ok, err := s.GetRepoAnswer(ctx, key, now)
	if err != nil {
		return "", err
	}
	if !ok {
		return "no stored issue row", nil
	}
	drop := func(why string) (string, error) {
		return why + "; dropped the stored issue", s.InvalidateRepoAnswersForSubject(ctx, owner, repo, RepoAnswerKindIssue, key.RefID)
	}
	if stored.Status != 200 {
		return drop("stored answer is a verdict")
	}
	sv, ok := decodeJSON([]byte(stored.Doc))
	if !ok {
		return drop("stored issue unreadable")
	}
	merged, ok := sv.(map[string]any)
	if !ok {
		return drop("stored issue unreadable")
	}
	newAt, err1 := time.Parse(time.RFC3339, payload["updated_at"].(string))
	oldAt, err2 := time.Parse(time.RFC3339, stringField(merged, "updated_at"))
	if err1 == nil && err2 == nil && oldAt.After(newAt) {
		return "stored issue is newer than the delivery", nil
	}
	if _, has := payload["closed_by"]; !has && stringField(payload, "state") != stringField(merged, "state") {
		// A close names who closed it only on the REST read.
		if stringField(payload, "state") != "open" {
			return drop("closed_by not in the payload")
		}
		merged["closed_by"] = nil
	}
	for k, v := range payload {
		merged[k] = v
	}
	doc, err := MarshalCacheDoc(stripURLKeys(merged))
	if err != nil {
		return drop("render failed")
	}
	stored.Doc = string(doc)
	stored.ExpiresAt = now.Add(IssueCacheTTL)
	stored.Subject = key.RefID
	if err := s.PutRepoAnswer(ctx, key, stored, now); err != nil {
		return "", err
	}
	return "rewrote the stored issue", nil
}

func stringField(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// InvalidateIssueAnswers drops the single-issue row for number and the
// repo's issue listings.
func (s *Store) InvalidateIssueAnswers(ctx context.Context, owner, repo string, number int64) error {
	if err := s.InvalidateRepoAnswersOfKind(ctx, owner, repo, RepoAnswerKindIssuesList); err != nil {
		return err
	}
	if number <= 0 {
		return nil
	}
	return s.InvalidateRepoAnswersForSubject(ctx, owner, repo, RepoAnswerKindIssue, strconv.FormatInt(number, 10))
}
