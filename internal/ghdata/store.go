package ghdata

import (
	"context"
	"database/sql"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Store wraps sqlc-generated queries and adds transaction logic for bulk
// operations against the ONE GLOBAL TRUTH STORE. State tables hold one row per
// resource; nothing here is scoped to a caller. What a caller may READ is the
// reveal layer's job (the grant/deny/visibility methods below plus the checks
// in internal/api).
type Store struct {
	db *sql.DB
	q  *dbgen.Queries
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, q: dbgen.New(db)}
}

// GrantTTL is how long an access grant stays valid without being re-earned.
// Long enough that steady callers never notice (every list-sync and every
// probe 2xx renews), short enough that revoked access ages out within a day
// even if GitHub never gives us an authoritative 403. Variable for tests.
var GrantTTL = 24 * time.Hour

// DenyTTL is how long an authoritative deny verdict (404 / non-rate-limit 403)
// is served before the same principal's request is probed against GitHub
// again. Deliberately short: it only exists to keep a repeatedly-poked
// unauthorized resource from hammering GitHub. Variable for tests.
var DenyTTL = 5 * time.Minute

// reconcileGrace protects webhook-absorbed truth from a racing fetch: an org
// or pulls fetch only deletes an open-PR row absent from its snapshot when the
// row was not touched (webhook-applied or otherwise written) after
// fetchStart - reconcileGrace. GraphQL/REST list reads are eventually
// consistent, so a just-webhooked PR can be missing from a snapshot taken
// moments later; without the grace window the reconcile would silently delete
// it (this race was actually hit during prototyping).
const reconcileGrace = 2 * time.Minute

// Repo visibility values (repos.visibility). Empty means unknown: the
// identity-locked GraphQL org fetch cannot carry visibility, so a repo seeded
// only by it stays unknown and the reveal layer treats it as private
// (fail closed) until a webhook or REST absorb reveals the real value.
const (
	VisibilityUnknown = ""
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Grant sources.
const (
	GrantSourceListSync = "list_sync"
	GrantSourceProbe    = "probe"
)

// ---- Repos ----

func (s *Store) GetRepo(ctx context.Context, owner, name string) (dbgen.Repo, error) {
	return s.q.GetRepo(ctx, dbgen.GetRepoParams{Owner: owner, Name: name})
}

func (s *Store) ListReposByOwner(ctx context.Context, owner string) ([]dbgen.Repo, error) {
	return s.q.ListReposByOwner(ctx, owner)
}

// ListVisibleReposByOwner returns the owner's repos the reveal layer permits
// for one principal: public repos plus repos with an unexpired grant.
func (s *Store) ListVisibleReposByOwner(ctx context.Context, owner, principal string, now time.Time) ([]dbgen.Repo, error) {
	return s.q.ListVisibleReposByOwner(ctx, dbgen.ListVisibleReposByOwnerParams{
		Owner:     owner,
		Principal: principal,
		ExpiresAt: rfc3339(now),
	})
}

func (s *Store) UpsertRepo(ctx context.Context, r dbgen.Repo) error {
	return s.q.UpsertRepo(ctx, repoToParams(r))
}

// DeleteRepoCascade removes a repo and everything hanging off it: PRs, labels,
// commit checks, cached contents, and every principal's grants for it. Used
// when a repository webhook reports the repo deleted (or renamed away).
func (s *Store) DeleteRepoCascade(ctx context.Context, owner, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeleteRepo(ctx, dbgen.DeleteRepoParams{Owner: owner, Name: name}); err != nil {
		return err
	}
	if err := q.DeletePullRequestsByRepo(ctx, dbgen.DeletePullRequestsByRepoParams{Owner: owner, Repo: name}); err != nil {
		return err
	}
	if err := q.DeletePRLabelsByRepo(ctx, dbgen.DeletePRLabelsByRepoParams{Owner: owner, Repo: name}); err != nil {
		return err
	}
	if err := q.DeleteCommitChecksByRepo(ctx, dbgen.DeleteCommitChecksByRepoParams{Owner: owner, Repo: name}); err != nil {
		return err
	}
	if err := q.DeleteContentsCacheByRepo(ctx, dbgen.DeleteContentsCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(name),
	}); err != nil {
		return err
	}
	if err := q.DeleteGrantsByRepo(ctx, dbgen.DeleteGrantsByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(name),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetRepoVisibility(ctx context.Context, owner, name, visibility string) error {
	return s.q.SetRepoVisibility(ctx, dbgen.SetRepoVisibilityParams{
		Visibility: visibility, Owner: owner, Name: name,
	})
}

func (s *Store) SetRepoArchived(ctx context.Context, owner, name string, archived bool) error {
	v := int64(0)
	if archived {
		v = 1
	}
	return s.q.SetRepoArchived(ctx, dbgen.SetRepoArchivedParams{IsArchived: v, Owner: owner, Name: name})
}

// ---- Org truth sync (the fetch write path) ----

// SyncOrgTruth merges an org-repos fetch into global truth and replace-syncs
// the fetching principal's grants for the owner. It is UPSERT-plus-guarded-
// reconcile, never delete-then-insert:
//
//   - Repo rows are only ever upserted. A fetch is one principal's PARTIAL
//     view (it omits repos that principal cannot see, and archived repos), so
//     absence from a fetch can never prove a repo is gone; deletion authority
//     belongs to repository webhooks (DeleteRepoCascade).
//   - Open-PR rows are reconciled per FETCHED repo (the fetch's open-PR list
//     for a repo it returned is complete): rows absent from the snapshot are
//     deleted, EXCEPT rows touched after fetchStart - reconcileGrace, so a
//     webhook racing the fetch's eventual consistency is never clobbered.
//   - Every repo the fetch returned earns the principal a list_sync grant
//     (replace-synced: absence from the new list revokes the old list_sync
//     grant); the principal's deny verdicts for the owner are cleared.
//
// fetchStart is when the fetch began (grace-window anchor); now stamps rows.
func (s *Store) SyncOrgTruth(ctx context.Context, owner string, data OrgSyncData, principal string, fetchStart, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	touched := rfc3339(now)
	cutoff := rfc3339(fetchStart.Add(-reconcileGrace))

	for _, r := range data.Repos {
		if err := q.UpsertRepo(ctx, repoToParams(r)); err != nil {
			return err
		}
	}

	for _, r := range data.Repos {
		repoKey := r.NameWithOwner
		prs := data.PRsByRepo[repoKey]
		labelsByNumber := data.LabelsByPR[repoKey]

		fetched := make(map[int64]bool, len(prs))
		for _, pr := range prs {
			fetched[pr.Number] = true
			if err := upsertPRTx(ctx, q, pr, touched); err != nil {
				return err
			}
			if err := replacePRLabelsTx(ctx, q, r.Owner, r.Name, pr.Number, labelsByNumber[pr.Number]); err != nil {
				return err
			}
		}

		// Reconcile: drop cached-open PRs the snapshot no longer lists, unless
		// they were touched inside the grace window (racing webhook wins).
		existing, err := q.ListOpenPullRequestNumbersByRepo(ctx, dbgen.ListOpenPullRequestNumbersByRepoParams{
			Owner: r.Owner, Repo: r.Name,
		})
		if err != nil {
			return err
		}
		for _, row := range existing {
			if fetched[row.Number] || row.TouchedAt >= cutoff {
				continue
			}
			if err := q.DeletePullRequest(ctx, dbgen.DeletePullRequestParams{Owner: r.Owner, Repo: r.Name, Number: row.Number}); err != nil {
				return err
			}
			if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{Owner: r.Owner, Repo: r.Name, PrNumber: row.Number}); err != nil {
				return err
			}
		}
	}

	// Grants: the fetch ran with the principal's own token, so every repo it
	// returned is GitHub's proof that this principal can read it.
	if principal != "" {
		if err := q.DeleteListSyncGrants(ctx, dbgen.DeleteListSyncGrantsParams{
			Principal: principal, Owner: NormalizeRepoKey(owner),
		}); err != nil {
			return err
		}
		for _, r := range data.Repos {
			if err := q.UpsertAccessGrant(ctx, dbgen.UpsertAccessGrantParams{
				Principal: principal,
				Owner:     NormalizeRepoKey(r.Owner),
				Repo:      NormalizeRepoKey(r.Name),
				GrantedAt: rfc3339(now),
				ExpiresAt: rfc3339(now.Add(GrantTTL)),
				Source:    GrantSourceListSync,
			}); err != nil {
				return err
			}
		}
		if err := q.DeleteDenialsByPrincipalOwner(ctx, dbgen.DeleteDenialsByPrincipalOwnerParams{
			Principal: principal, Owner: NormalizeRepoKey(owner),
		}); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// OrgSyncData is one org-repos fetch's snapshot, ready to merge into truth.
type OrgSyncData struct {
	Repos      []dbgen.Repo
	PRsByRepo  map[string][]dbgen.PullRequest       // key: "owner/repo"
	LabelsByPR map[string]map[int64][]dbgen.PrLabel // key: "owner/repo" -> pr number -> labels
}

// ---- Pull Requests ----

func (s *Store) GetPullRequest(ctx context.Context, owner, repo string, number int64) (dbgen.PullRequest, error) {
	return s.q.GetPullRequest(ctx, dbgen.GetPullRequestParams{Owner: owner, Repo: repo, Number: number})
}

func (s *Store) ListOpenPRsByRepo(ctx context.Context, owner, repo string) ([]dbgen.PullRequest, error) {
	return s.q.ListOpenPullRequestsByRepo(ctx, dbgen.ListOpenPullRequestsByRepoParams{Owner: owner, Repo: repo})
}

// UpsertPR merges one source's view of a PR into truth (see the query comment
// for the COALESCE semantics), stamping touched_at.
func (s *Store) UpsertPR(ctx context.Context, pr dbgen.PullRequest, now time.Time) error {
	return upsertPRTx(ctx, s.q, pr, rfc3339(now))
}

// UpsertPRWithChecks upserts a PR plus its labels and re-derives
// last_commit_status from the commit checks already recorded for the PR's head
// commit: a PR payload carries no CI state, so a PR first seen AFTER its head
// commit's checks finished (e.g. a pr-minder auto-opened PR) would otherwise
// stay NULL until a later check event. When no checks are recorded the
// (COALESCE-preserved) status is left untouched.
func (s *Store) UpsertPRWithChecks(ctx context.Context, pr dbgen.PullRequest, labels []dbgen.PrLabel, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := upsertPRTx(ctx, q, pr, rfc3339(now)); err != nil {
		return err
	}
	if pr.HeadRefOid.Valid && pr.HeadRefOid.String != "" {
		states, err := q.ListCommitCheckStates(ctx, dbgen.ListCommitCheckStatesParams{
			Owner: pr.Owner, Repo: pr.Repo, Sha: pr.HeadRefOid.String,
		})
		if err != nil {
			return err
		}
		if rollup := rollupState(states); rollup != "" {
			if err := q.SetPRStatusByHeadSha(ctx, dbgen.SetPRStatusByHeadShaParams{
				LastCommitStatus: sql.NullString{String: rollup, Valid: true},
				Owner:            pr.Owner,
				Repo:             pr.Repo,
				HeadRefOid:       pr.HeadRefOid,
			}); err != nil {
				return err
			}
		}
	}
	if err := replacePRLabelsTx(ctx, q, pr.Owner, pr.Repo, pr.Number, labels); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletePR removes a PR and its labels (a closed/merged PR leaves the cache).
func (s *Store) DeletePR(ctx context.Context, owner, repo string, number int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{Owner: owner, Repo: repo, PrNumber: number}); err != nil {
		return err
	}
	if err := q.DeletePullRequest(ctx, dbgen.DeletePullRequestParams{Owner: owner, Repo: repo, Number: number}); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetPRMergeable marks one PR's mergeable/mergeable_state/merge_commit_sha
// as unknown: its head moved, so GitHub is recomputing them and the cached
// values are stale. The /pulls/{n} known-mergeable gate misses on NULL.
func (s *Store) ResetPRMergeable(ctx context.Context, owner, repo string, number int64) error {
	return s.q.ResetPRMergeable(ctx, dbgen.ResetPRMergeableParams{Owner: owner, Repo: repo, Number: number})
}

// ResetMergeableByBaseRef marks every open PR targeting the just-pushed base
// branch as mergeability-unknown (the base moved under them).
func (s *Store) ResetMergeableByBaseRef(ctx context.Context, owner, repo, baseRef string) error {
	return s.q.ResetMergeableByBaseRef(ctx, dbgen.ResetMergeableByBaseRefParams{
		Owner: owner, Repo: repo, BaseRefName: sql.NullString{String: baseRef, Valid: baseRef != ""},
	})
}

// ---- PR Labels ----

func (s *Store) SetPRLabels(ctx context.Context, owner, repo string, prNumber int64, labels []dbgen.PrLabel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err := replacePRLabelsTx(ctx, q, owner, repo, prNumber, labels); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListPRLabels(ctx context.Context, owner, repo string, prNumber int64) ([]dbgen.PrLabel, error) {
	return s.q.ListPRLabels(ctx, dbgen.ListPRLabelsParams{Owner: owner, Repo: repo, PrNumber: prNumber})
}

// RecolorPRLabel updates the color of a label across all PRs in a repo.
func (s *Store) RecolorPRLabel(ctx context.Context, owner, repo, name, color string) error {
	return s.q.SetPRLabelColorByName(ctx, dbgen.SetPRLabelColorByNameParams{
		Color: color, Owner: owner, Repo: repo, Name: name,
	})
}

// DeletePRLabelByName removes a label from all PRs in a repo.
func (s *Store) DeletePRLabelByName(ctx context.Context, owner, repo, name string) error {
	return s.q.DeletePRLabelsByName(ctx, dbgen.DeletePRLabelsByNameParams{Owner: owner, Repo: repo, Name: name})
}

// ---- Commit checks ----

// ApplyCommitStatus records a single check/status state for a commit and
// recomputes the rollup, writing it onto any PR whose head is that commit and,
// when onDefaultBranch is set, onto the repo's default_branch_status. Returns
// the resulting rollup state.
func (s *Store) ApplyCommitStatus(ctx context.Context, owner, repo, sha, checkContext, state string, onDefaultBranch bool) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.UpsertCommitCheck(ctx, dbgen.UpsertCommitCheckParams{
		Owner: owner, Repo: repo, Sha: sha, Context: checkContext, State: state,
	}); err != nil {
		return "", err
	}
	states, err := q.ListCommitCheckStates(ctx, dbgen.ListCommitCheckStatesParams{
		Owner: owner, Repo: repo, Sha: sha,
	})
	if err != nil {
		return "", err
	}
	rollup := rollupState(states)
	status := sql.NullString{String: rollup, Valid: rollup != ""}
	if err := q.SetPRStatusByHeadSha(ctx, dbgen.SetPRStatusByHeadShaParams{
		LastCommitStatus: status,
		Owner:            owner,
		Repo:             repo,
		HeadRefOid:       sql.NullString{String: sha, Valid: sha != ""},
	}); err != nil {
		return "", err
	}
	if onDefaultBranch {
		if err := q.SetRepoDefaultBranchStatus(ctx, dbgen.SetRepoDefaultBranchStatusParams{
			DefaultBranchStatus: status,
			Owner:               owner,
			Name:                repo,
		}); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return rollup, nil
}

// DeleteCommitChecks drops the per-check rows for a commit (e.g. when the PR
// that pointed at it closes).
func (s *Store) DeleteCommitChecks(ctx context.Context, owner, repo, sha string) error {
	if sha == "" {
		return nil
	}
	return s.q.DeleteCommitChecksBySha(ctx, dbgen.DeleteCommitChecksByShaParams{Owner: owner, Repo: repo, Sha: sha})
}

// SetRepoPushedAt updates a repo's pushed_at.
func (s *Store) SetRepoPushedAt(ctx context.Context, owner, repo, pushedAt string) error {
	return s.q.SetRepoPushedAt(ctx, dbgen.SetRepoPushedAtParams{
		PushedAt: sql.NullString{String: pushedAt, Valid: pushedAt != ""},
		Owner:    owner, Name: repo,
	})
}

// ---- Access grants + deny verdicts (the reveal layer) ----

// RecordGrant stores GitHub's proof that a principal can read a repo, renewing
// the TTL, and clears the principal's deny verdicts for that repo (a fresh
// grant supersedes a stale "no"). owner/repo are normalized here.
func (s *Store) RecordGrant(ctx context.Context, principal, owner, repo, source string, now time.Time) error {
	owner, repo = NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err := q.UpsertAccessGrant(ctx, dbgen.UpsertAccessGrantParams{
		Principal: principal, Owner: owner, Repo: repo,
		GrantedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(GrantTTL)), Source: source,
	}); err != nil {
		return err
	}
	if err := q.DeleteDenialsByPrincipalRepo(ctx, dbgen.DeleteDenialsByPrincipalRepoParams{
		Principal: principal, Owner: owner, Repo: repo,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// HasGrant reports whether the principal holds an unexpired grant for the repo.
func (s *Store) HasGrant(ctx context.Context, principal, owner, repo string, now time.Time) (bool, error) {
	g, err := s.q.GetAccessGrant(ctx, dbgen.GetAccessGrantParams{
		Principal: principal, Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return g.ExpiresAt > rfc3339(now), nil
}

// RevokeGrant deletes a principal's grant for a repo (an authoritative 403
// said their access is gone).
func (s *Store) RevokeGrant(ctx context.Context, principal, owner, repo string) error {
	return s.q.DeleteAccessGrant(ctx, dbgen.DeleteAccessGrantParams{
		Principal: principal, Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// DenyVerdict is a cached authoritative "no" from GitHub for one principal's
// probe of one resource.
type DenyVerdict struct {
	Status  int
	Message string
}

// RecordDenyVerdict caches GitHub's authoritative 404/403 answer to one
// principal's probe of one resource, for DenyTTL.
func (s *Store) RecordDenyVerdict(ctx context.Context, principal, kind, key, owner, repo string, status int, message string, now time.Time) error {
	return s.q.UpsertDenyVerdict(ctx, dbgen.UpsertDenyVerdictParams{
		Principal: principal, ResourceKind: kind, ResourceKey: key,
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		Status: int64(status), Message: message,
		DeniedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(DenyTTL)),
	})
}

// GetDenyVerdict returns the unexpired deny verdict for one principal's
// resource, or (zero, false) when none applies.
func (s *Store) GetDenyVerdict(ctx context.Context, principal, kind, key string, now time.Time) (DenyVerdict, bool, error) {
	row, err := s.q.GetDenyVerdict(ctx, dbgen.GetDenyVerdictParams{
		Principal: principal, ResourceKind: kind, ResourceKey: key,
	})
	if err == sql.ErrNoRows {
		return DenyVerdict{}, false, nil
	}
	if err != nil {
		return DenyVerdict{}, false, err
	}
	if row.ExpiresAt <= rfc3339(now) {
		return DenyVerdict{}, false, nil
	}
	return DenyVerdict{Status: int(row.Status), Message: row.Message}, true, nil
}

// PruneAccessControl deletes expired grants and deny verdicts. Called
// opportunistically from write paths; cheap either way.
func (s *Store) PruneAccessControl(ctx context.Context, now time.Time) error {
	if err := s.q.DeleteExpiredGrants(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.DeleteExpiredDenials(ctx, rfc3339(now))
}

// ---- shared helpers ----

func upsertPRTx(ctx context.Context, q *dbgen.Queries, pr dbgen.PullRequest, touchedAt string) error {
	p := prToParams(pr)
	p.TouchedAt = touchedAt
	return q.UpsertPullRequest(ctx, p)
}

func replacePRLabelsTx(ctx context.Context, q *dbgen.Queries, owner, repo string, prNumber int64, labels []dbgen.PrLabel) error {
	if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{Owner: owner, Repo: repo, PrNumber: prNumber}); err != nil {
		return err
	}
	for _, l := range labels {
		if err := q.InsertPRLabel(ctx, dbgen.InsertPRLabelParams{
			Owner:    l.Owner,
			Repo:     l.Repo,
			PrNumber: l.PrNumber,
			Name:     l.Name,
			Color:    l.Color,
		}); err != nil {
			return err
		}
	}
	return nil
}

// rollupState aggregates per-check states into a single GitHub-style rollup:
// any failure dominates, then pending, then success.
//
// TODO: a missed check-completion delivery leaves its commit_checks row stuck at
// PENDING, pinning the rollup at PENDING even after every real check finished.
// Reconciling stuck-PENDING rows (e.g. re-reading the sha's checks from GitHub)
// is deliberately deferred -- do not add it here without that design discussion.
func rollupState(states []string) string {
	var hasFailure, hasPending, hasSuccess bool
	for _, st := range states {
		switch st {
		case "FAILURE", "ERROR":
			hasFailure = true
		case "PENDING", "EXPECTED":
			hasPending = true
		case "SUCCESS":
			hasSuccess = true
		}
	}
	switch {
	case hasFailure:
		return "FAILURE"
	case hasPending:
		return "PENDING"
	case hasSuccess:
		return "SUCCESS"
	default:
		return ""
	}
}

func repoToParams(r dbgen.Repo) dbgen.UpsertRepoParams {
	return dbgen.UpsertRepoParams{
		Owner:               r.Owner,
		Name:                r.Name,
		NameWithOwner:       r.NameWithOwner,
		Url:                 r.Url,
		IsDisabled:          r.IsDisabled,
		IsArchived:          r.IsArchived,
		Visibility:          r.Visibility,
		PushedAt:            r.PushedAt,
		DefaultBranch:       r.DefaultBranch,
		DefaultBranchStatus: r.DefaultBranchStatus,
		OwnerLogin:          r.OwnerLogin,
		OwnerAvatar:         r.OwnerAvatar,
		OwnerUrl:            r.OwnerUrl,
	}
}

func prToParams(pr dbgen.PullRequest) dbgen.UpsertPullRequestParams {
	return dbgen.UpsertPullRequestParams{
		Owner:              pr.Owner,
		Repo:               pr.Repo,
		Number:             pr.Number,
		Title:              pr.Title,
		Url:                pr.Url,
		IsDraft:            pr.IsDraft,
		State:              pr.State,
		CreatedAt:          pr.CreatedAt,
		UpdatedAt:          pr.UpdatedAt,
		Additions:          pr.Additions,
		Deletions:          pr.Deletions,
		Mergeable:          pr.Mergeable,
		AuthorLogin:        pr.AuthorLogin,
		AuthorAvatar:       pr.AuthorAvatar,
		AuthorUrl:          pr.AuthorUrl,
		HeadRefName:        pr.HeadRefName,
		BaseRefName:        pr.BaseRefName,
		HeadRefOid:         pr.HeadRefOid,
		ReviewRequestCount: pr.ReviewRequestCount,
		LastCommitStatus:   pr.LastCommitStatus,
		NodeID:             pr.NodeID,
		Body:               pr.Body,
		AutoMerge:          pr.AutoMerge,
		MergeableState:     pr.MergeableState,
		MergeCommitSha:     pr.MergeCommitSha,
		BaseSha:            pr.BaseSha,
		HeadRepoFullName:   pr.HeadRepoFullName,
		TouchedAt:          pr.TouchedAt,
	}
}
