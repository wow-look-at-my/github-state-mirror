// State for the self-hosted runner reads other than the org listing; see docs/cache/rest-routes.md.

package ghdata

import _ "embed"

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// State for the self-hosted runner reads other than the org listing; see docs/cache/rest-routes.md.

// RunnerGoneTTL bounds a stored absent-runner verdict.
const RunnerGoneTTL = 5 * time.Minute

// OrgRunnerScope is the scope of an org runner.
func OrgRunnerScope(org string) string { return "org:" + NormalizeRepoKey(org) }

// RepoRunnerScope is the scope of a repo runner and of the repo listing.
func RepoRunnerScope(owner, repo string) string {
	return "repo:" + NormalizeRepoKey(owner) + "/" + NormalizeRepoKey(repo)
}

// RunnerAnswerKey addresses one stored runner answer.
type RunnerAnswerKey struct {
	TokenFP  string
	Scope    string
	RunnerID int64
	PerPage  int64
	Page     int64
}

func (k RunnerAnswerKey) args() []any {
	return []any{k.TokenFP, k.Scope, k.RunnerID, k.PerPage, k.Page}
}

const runnerAnswerKeyWhere = `token_fp = ? AND scope = ? AND runner_id = ? AND per_page = ? AND page = ?`

// GetRunnerAnswer returns the stored status and document, or false on a miss
// or an expired row. A hit refreshes the LRU stamp of the row.
func (s *Store) GetRunnerAnswer(ctx context.Context, k RunnerAnswerKey, now time.Time) (int, string, bool, error) {
	var status int
	var doc, expires string
	err := s.dbx(ctx).QueryRowContext(ctx,
		`SELECT status, doc, expires_at FROM runner_answer_cache WHERE `+runnerAnswerKeyWhere, k.args()...,
	).Scan(&status, &doc, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	if exp, perr := time.Parse(time.RFC3339, expires); perr != nil || !exp.After(now) {
		return 0, "", false, nil
	}
	_, _ = s.dbx(ctx).ExecContext(ctx,
		`UPDATE runner_answer_cache SET last_used_at = ? WHERE `+runnerAnswerKeyWhere,
		append([]any{rfc3339(now)}, k.args()...)...)
	return status, doc, true, nil
}

// PutRunnerAnswer records an answer. Then it prunes expired rows and the LRU
// tail beyond the cap.
func (s *Store) PutRunnerAnswer(ctx context.Context, k RunnerAnswerKey, status int, doc string, now time.Time, ttl time.Duration) error {
	db := s.dbx(ctx)
	if _, err := db.ExecContext(ctx, putRunnerAnswerSql,
		append(k.args(), status, doc, rfc3339(now), rfc3339(now.Add(ttl)), rfc3339(now))...,
	); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM runner_answer_cache WHERE expires_at <= ?`, rfc3339(now)); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, putRunnerAnswerSql2, CacheMaxRows)
	return err
}

// InvalidateRunner drops the answers of every credential that a job on this
// runner moves. This covers the rows of the runner in the org and repo
// scopes, the repo listing, and the org listing.
func (s *Store) InvalidateRunner(ctx context.Context, owner, repo string, runnerID int64) error {
	db := s.dbx(ctx)
	if _, err := db.ExecContext(ctx,
		`DELETE FROM runner_answer_cache WHERE (scope = ? AND runner_id = ?) OR (scope = ? AND runner_id IN (?, 0))`,
		OrgRunnerScope(owner), runnerID, RepoRunnerScope(owner, repo), runnerID); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM org_runners_cache WHERE org = ?`, NormalizeRepoKey(owner))
	return err
}

//go:embed put-runner-answer.sql
var putRunnerAnswerSql string

//go:embed put-runner-answer-2.sql
var putRunnerAnswerSql2 string
