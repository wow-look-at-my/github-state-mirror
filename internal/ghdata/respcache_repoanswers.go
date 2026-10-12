package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The routes on repo_answer_cache; see docs/cache/rest-routes.md.
const (
	RepoAnswerKindTags    = "tags"
	RepoAnswerKindRunLogs = "run_logs"
	RepoAnswerKindJobLogs = "job_logs"
	RepoAnswerKindCommit = "commit"
	RepoAnswerKindBranchesWhereHead = "branches_where_head"
)

// RepoAnswerKey addresses one stored answer.
type RepoAnswerKey struct {
	Owner string
	Repo  string
	Kind  string // RepoAnswerKind*
	RefID string // the resource inside the repo, verbatim
	Shape string // canonical modeled query
}

func (k RepoAnswerKey) args() []any {
	return []any{NormalizeRepoKey(k.Owner), NormalizeRepoKey(k.Repo), k.Kind, k.RefID, k.Shape}
}

// RepoAnswer is one stored answer. Subject is what a delivery names to
// flush it.
type RepoAnswer struct {
	Status    int
	Doc       string
	Subject   string
	ExpiresAt time.Time
}

const repoAnswerKeyWhere = `owner = ? AND repo = ? AND kind = ? AND ref_id = ? AND shape = ?`

// GetRepoAnswer returns the stored answer, or false on a miss or an expired
// row. A hit refreshes the row's LRU stamp.
func (s *Store) GetRepoAnswer(ctx context.Context, k RepoAnswerKey, now time.Time) (RepoAnswer, bool, error) {
	var a RepoAnswer
	var expires string
	err := s.dbx(ctx).QueryRowContext(ctx,
		`SELECT status, doc, subject, expires_at FROM repo_answer_cache WHERE `+repoAnswerKeyWhere, k.args()...,
	).Scan(&a.Status, &a.Doc, &a.Subject, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return RepoAnswer{}, false, nil
	}
	if err != nil {
		return RepoAnswer{}, false, err
	}
	exp, perr := time.Parse(time.RFC3339, expires)
	if perr != nil || !exp.After(now) {
		return RepoAnswer{}, false, nil
	}
	a.ExpiresAt = exp
	_, _ = s.dbx(ctx).ExecContext(ctx,
		`UPDATE repo_answer_cache SET last_used_at = ? WHERE `+repoAnswerKeyWhere,
		append([]any{rfc3339(now)}, k.args()...)...)
	return a, true, nil
}

// PutRepoAnswer records an answer, then prunes expired rows and the LRU tail
// beyond the cap.
func (s *Store) PutRepoAnswer(ctx context.Context, k RepoAnswerKey, a RepoAnswer, now time.Time) error {
	db := s.dbx(ctx)
	if _, err := db.ExecContext(ctx, `
INSERT INTO repo_answer_cache (owner, repo, kind, ref_id, shape, subject, status, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, kind, ref_id, shape) DO UPDATE SET
    subject = excluded.subject,
    status = excluded.status,
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at`,
		append(k.args(), a.Subject, a.Status, a.Doc, rfc3339(now), rfc3339(a.ExpiresAt), rfc3339(now))...,
	); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM repo_answer_cache WHERE expires_at <= ?`, rfc3339(now)); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
DELETE FROM repo_answer_cache WHERE id IN (
    SELECT id FROM repo_answer_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
)`, CacheMaxRows)
	return err
}

// StoredRepoAnswer is one listed row, for a delivery that rewrites rows in
// place.
type StoredRepoAnswer struct {
	Key RepoAnswerKey
	RepoAnswer
}

// ListRepoAnswersOfKind returns a repo's unexpired rows of one kind.
func (s *Store) ListRepoAnswersOfKind(ctx context.Context, owner, repo, kind string, now time.Time) ([]StoredRepoAnswer, error) {
	rows, err := s.dbx(ctx).QueryContext(ctx, `
SELECT owner, repo, kind, ref_id, shape, status, doc, subject, expires_at
FROM repo_answer_cache WHERE owner = ? AND repo = ? AND kind = ? AND expires_at > ?`,
		NormalizeRepoKey(owner), NormalizeRepoKey(repo), kind, rfc3339(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredRepoAnswer
	for rows.Next() {
		var a StoredRepoAnswer
		var expires string
		if err := rows.Scan(&a.Key.Owner, &a.Key.Repo, &a.Key.Kind, &a.Key.RefID, &a.Key.Shape,
			&a.Status, &a.Doc, &a.Subject, &expires); err != nil {
			return nil, err
		}
		a.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
		out = append(out, a)
	}
	return out, rows.Err()
}

// InvalidateRepoAnswers drops every stored answer for a repo, every kind.
func (s *Store) InvalidateRepoAnswers(ctx context.Context, owner, repo string) error {
	_, err := s.dbx(ctx).ExecContext(ctx, `DELETE FROM repo_answer_cache WHERE owner = ? AND repo = ?`,
		NormalizeRepoKey(owner), NormalizeRepoKey(repo))
	return err
}

// InvalidateRepoAnswersOfKind drops a repo's stored answers of one kind.
func (s *Store) InvalidateRepoAnswersOfKind(ctx context.Context, owner, repo, kind string) error {
	_, err := s.dbx(ctx).ExecContext(ctx, `DELETE FROM repo_answer_cache WHERE owner = ? AND repo = ? AND kind = ?`,
		NormalizeRepoKey(owner), NormalizeRepoKey(repo), kind)
	return err
}

// InvalidateRepoAnswersForSubject drops the rows of one kind a delivery's
// subject names.
func (s *Store) InvalidateRepoAnswersForSubject(ctx context.Context, owner, repo, kind, subject string) error {
	_, err := s.dbx(ctx).ExecContext(ctx,
		`DELETE FROM repo_answer_cache WHERE owner = ? AND repo = ? AND kind = ? AND subject = ?`,
		NormalizeRepoKey(owner), NormalizeRepoKey(repo), kind, subject)
	return err
}
