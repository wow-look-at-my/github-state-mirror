package ghdata

import (
	"context"
	"database/sql"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Store wraps sqlc-generated queries and adds transaction logic for bulk operations.
type Store struct {
	db *sql.DB
	q  *dbgen.Queries
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, q: dbgen.New(db)}
}

// ---- Users ----

func (s *Store) UpsertUser(ctx context.Context, u dbgen.User) error {
	return s.q.UpsertUser(ctx, dbgen.UpsertUserParams{
		Login:     u.Login,
		AvatarUrl: u.AvatarUrl,
		Url:       u.Url,
	})
}

func (s *Store) GetFirstUser(ctx context.Context) (dbgen.User, error) {
	return s.q.GetFirstUser(ctx)
}

// ---- Orgs ----

func (s *Store) UpsertOrg(ctx context.Context, o dbgen.Org) error {
	return s.q.UpsertOrg(ctx, dbgen.UpsertOrgParams{
		Login:     o.Login,
		AvatarUrl: o.AvatarUrl,
		Url:       o.Url,
	})
}

func (s *Store) ListOrgs(ctx context.Context) ([]dbgen.Org, error) {
	return s.q.ListOrgs(ctx)
}

// SetUserOrgs replaces the full set of org memberships + org records for a user.
func (s *Store) SetUserOrgs(ctx context.Context, userLogin string, orgs []dbgen.Org) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeleteUserOrgMemberships(ctx, userLogin); err != nil {
		return err
	}
	for _, o := range orgs {
		if err := q.UpsertOrg(ctx, dbgen.UpsertOrgParams{
			Login:     o.Login,
			AvatarUrl: o.AvatarUrl,
			Url:       o.Url,
		}); err != nil {
			return err
		}
		if err := q.SetUserOrgMembership(ctx, dbgen.SetUserOrgMembershipParams{
			UserLogin: userLogin,
			OrgLogin:  o.Login,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListUserOrgs(ctx context.Context, userLogin string) ([]dbgen.Org, error) {
	logins, err := s.q.ListUserOrgMemberships(ctx, userLogin)
	if err != nil {
		return nil, err
	}
	var orgs []dbgen.Org
	for _, login := range logins {
		o, err := s.q.GetOrg(ctx, login)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		orgs = append(orgs, o)
	}
	return orgs, nil
}

// ---- Repos ----

// SetOrgRepos replaces all repos for an owner (org) in a single transaction.
func (s *Store) SetOrgRepos(ctx context.Context, owner string, repos []dbgen.Repo) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeleteReposByOwner(ctx, owner); err != nil {
		return err
	}
	for _, r := range repos {
		if err := q.UpsertRepo(ctx, repoToParams(r)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetRepo(ctx context.Context, owner, name string) (dbgen.Repo, error) {
	return s.q.GetRepo(ctx, dbgen.GetRepoParams{Owner: owner, Name: name})
}

func (s *Store) ListReposByOwner(ctx context.Context, owner string) ([]dbgen.Repo, error) {
	return s.q.ListReposByOwner(ctx, owner)
}

func (s *Store) UpsertRepo(ctx context.Context, r dbgen.Repo) error {
	return s.q.UpsertRepo(ctx, repoToParams(r))
}

// ---- Pull Requests ----

// SetRepoPRs replaces all PRs (and their labels) for a repo in a single transaction.
func (s *Store) SetRepoPRs(ctx context.Context, owner, repo string, prs []dbgen.PullRequest, labelsByPR map[int64][]dbgen.PrLabel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeletePullRequestsByRepo(ctx, dbgen.DeletePullRequestsByRepoParams{Owner: owner, Repo: repo}); err != nil {
		return err
	}
	if err := q.DeletePRLabelsByRepo(ctx, dbgen.DeletePRLabelsByRepoParams{Owner: owner, Repo: repo}); err != nil {
		return err
	}
	for _, pr := range prs {
		if err := q.UpsertPullRequest(ctx, prToParams(pr)); err != nil {
			return err
		}
		if labels, ok := labelsByPR[pr.Number]; ok {
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
		}
	}
	return tx.Commit()
}

func (s *Store) GetPullRequest(ctx context.Context, owner, repo string, number int64) (dbgen.PullRequest, error) {
	return s.q.GetPullRequest(ctx, dbgen.GetPullRequestParams{Owner: owner, Repo: repo, Number: number})
}

func (s *Store) ListOpenPRsByRepo(ctx context.Context, owner, repo string) ([]dbgen.PullRequest, error) {
	return s.q.ListOpenPullRequestsByRepo(ctx, dbgen.ListOpenPullRequestsByRepoParams{Owner: owner, Repo: repo})
}

func (s *Store) ListOpenPRsByOwner(ctx context.Context, owner string) ([]dbgen.PullRequest, error) {
	return s.q.ListOpenPullRequestsByOwner(ctx, owner)
}

func (s *Store) UpsertPR(ctx context.Context, pr dbgen.PullRequest) error {
	return s.q.UpsertPullRequest(ctx, prToParams(pr))
}

// ---- PR Labels ----

func (s *Store) SetPRLabels(ctx context.Context, owner, repo string, prNumber int64, labels []dbgen.PrLabel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

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
	return tx.Commit()
}

func (s *Store) ListPRLabels(ctx context.Context, owner, repo string, prNumber int64) ([]dbgen.PrLabel, error) {
	return s.q.ListPRLabels(ctx, dbgen.ListPRLabelsParams{Owner: owner, Repo: repo, PrNumber: prNumber})
}

// ---- PR Files ----

func (s *Store) SetPRFiles(ctx context.Context, owner, repo string, prNumber int64, files []dbgen.PrFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeletePRFiles(ctx, dbgen.DeletePRFilesParams{Owner: owner, Repo: repo, PrNumber: prNumber}); err != nil {
		return err
	}
	for _, f := range files {
		if err := q.InsertPRFile(ctx, dbgen.InsertPRFileParams{
			Owner:     f.Owner,
			Repo:      f.Repo,
			PrNumber:  f.PrNumber,
			Path:      f.Path,
			Additions: f.Additions,
			Deletions: f.Deletions,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListPRFiles(ctx context.Context, owner, repo string, prNumber int64) ([]dbgen.PrFile, error) {
	return s.q.ListPRFiles(ctx, dbgen.ListPRFilesParams{Owner: owner, Repo: repo, PrNumber: prNumber})
}

// ---- Branch Comparisons ----

func (s *Store) UpsertComparison(ctx context.Context, c dbgen.BranchComparison) error {
	return s.q.UpsertBranchComparison(ctx, dbgen.UpsertBranchComparisonParams{
		Owner:    c.Owner,
		Repo:     c.Repo,
		BaseRef:  c.BaseRef,
		HeadRef:  c.HeadRef,
		AheadBy:  c.AheadBy,
		BehindBy: c.BehindBy,
	})
}

func (s *Store) GetComparison(ctx context.Context, owner, repo, base, head string) (dbgen.BranchComparison, error) {
	return s.q.GetBranchComparison(ctx, dbgen.GetBranchComparisonParams{
		Owner:   owner,
		Repo:    repo,
		BaseRef: base,
		HeadRef: head,
	})
}

// ---- Helpers ----

func repoToParams(r dbgen.Repo) dbgen.UpsertRepoParams {
	return dbgen.UpsertRepoParams{
		Owner:               r.Owner,
		Name:                r.Name,
		NameWithOwner:       r.NameWithOwner,
		Url:                 r.Url,
		IsDisabled:          r.IsDisabled,
		IsArchived:          r.IsArchived,
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
	}
}
