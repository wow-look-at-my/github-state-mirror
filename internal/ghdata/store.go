package ghdata

import (
	"context"
	"database/sql"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
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
	act := actor.FromContext(ctx)
	return s.q.UpsertUser(ctx, dbgen.UpsertUserParams{
		Actor:     act,
		Login:     u.Login,
		AvatarUrl: u.AvatarUrl,
		Url:       u.Url,
	})
}

func (s *Store) GetFirstUser(ctx context.Context) (dbgen.User, error) {
	act := actor.FromContext(ctx)
	return s.q.GetFirstUser(ctx, act)
}

// ---- Orgs ----

func (s *Store) UpsertOrg(ctx context.Context, o dbgen.Org) error {
	act := actor.FromContext(ctx)
	return s.q.UpsertOrg(ctx, dbgen.UpsertOrgParams{
		Actor:     act,
		Login:     o.Login,
		AvatarUrl: o.AvatarUrl,
		Url:       o.Url,
	})
}

func (s *Store) ListOrgs(ctx context.Context) ([]dbgen.Org, error) {
	act := actor.FromContext(ctx)
	return s.q.ListOrgs(ctx, act)
}

// SetUserOrgs replaces the full set of org memberships + org records for a user.
func (s *Store) SetUserOrgs(ctx context.Context, userLogin string, orgs []dbgen.Org) error {
	act := actor.FromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeleteUserOrgMemberships(ctx, dbgen.DeleteUserOrgMembershipsParams{Actor: act, UserLogin: userLogin}); err != nil {
		return err
	}
	for _, o := range orgs {
		if err := q.UpsertOrg(ctx, dbgen.UpsertOrgParams{
			Actor:     act,
			Login:     o.Login,
			AvatarUrl: o.AvatarUrl,
			Url:       o.Url,
		}); err != nil {
			return err
		}
		if err := q.SetUserOrgMembership(ctx, dbgen.SetUserOrgMembershipParams{
			Actor:     act,
			UserLogin: userLogin,
			OrgLogin:  o.Login,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListUserOrgs(ctx context.Context, userLogin string) ([]dbgen.Org, error) {
	act := actor.FromContext(ctx)
	logins, err := s.q.ListUserOrgMemberships(ctx, dbgen.ListUserOrgMembershipsParams{Actor: act, UserLogin: userLogin})
	if err != nil {
		return nil, err
	}
	var orgs []dbgen.Org
	for _, login := range logins {
		o, err := s.q.GetOrg(ctx, dbgen.GetOrgParams{Actor: act, Login: login})
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
	act := actor.FromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeleteReposByOwner(ctx, dbgen.DeleteReposByOwnerParams{Actor: act, Owner: owner}); err != nil {
		return err
	}
	for _, r := range repos {
		if err := q.UpsertRepo(ctx, repoToParams(act, r)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetRepo(ctx context.Context, owner, name string) (dbgen.Repo, error) {
	act := actor.FromContext(ctx)
	return s.q.GetRepo(ctx, dbgen.GetRepoParams{Actor: act, Owner: owner, Name: name})
}

func (s *Store) ListReposByOwner(ctx context.Context, owner string) ([]dbgen.Repo, error) {
	act := actor.FromContext(ctx)
	return s.q.ListReposByOwner(ctx, dbgen.ListReposByOwnerParams{Actor: act, Owner: owner})
}

func (s *Store) UpsertRepo(ctx context.Context, r dbgen.Repo) error {
	act := actor.FromContext(ctx)
	return s.q.UpsertRepo(ctx, repoToParams(act, r))
}

// ---- Pull Requests ----

// SetRepoPRs replaces all PRs (and their labels) for a repo in a single transaction.
func (s *Store) SetRepoPRs(ctx context.Context, owner, repo string, prs []dbgen.PullRequest, labelsByPR map[int64][]dbgen.PrLabel) error {
	act := actor.FromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeletePullRequestsByRepo(ctx, dbgen.DeletePullRequestsByRepoParams{Actor: act, Owner: owner, Repo: repo}); err != nil {
		return err
	}
	if err := q.DeletePRLabelsByRepo(ctx, dbgen.DeletePRLabelsByRepoParams{Actor: act, Owner: owner, Repo: repo}); err != nil {
		return err
	}
	for _, pr := range prs {
		if err := q.UpsertPullRequest(ctx, prToParams(act, pr)); err != nil {
			return err
		}
		if labels, ok := labelsByPR[pr.Number]; ok {
			for _, l := range labels {
				if err := q.InsertPRLabel(ctx, dbgen.InsertPRLabelParams{
					Actor:    act,
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
	act := actor.FromContext(ctx)
	return s.q.GetPullRequest(ctx, dbgen.GetPullRequestParams{Actor: act, Owner: owner, Repo: repo, Number: number})
}

func (s *Store) ListOpenPRsByRepo(ctx context.Context, owner, repo string) ([]dbgen.PullRequest, error) {
	act := actor.FromContext(ctx)
	return s.q.ListOpenPullRequestsByRepo(ctx, dbgen.ListOpenPullRequestsByRepoParams{Actor: act, Owner: owner, Repo: repo})
}

func (s *Store) ListOpenPRsByOwner(ctx context.Context, owner string) ([]dbgen.PullRequest, error) {
	act := actor.FromContext(ctx)
	return s.q.ListOpenPullRequestsByOwner(ctx, dbgen.ListOpenPullRequestsByOwnerParams{Actor: act, Owner: owner})
}

func (s *Store) UpsertPR(ctx context.Context, pr dbgen.PullRequest) error {
	act := actor.FromContext(ctx)
	return s.q.UpsertPullRequest(ctx, prToParams(act, pr))
}

// ---- PR Labels ----

func (s *Store) SetPRLabels(ctx context.Context, owner, repo string, prNumber int64, labels []dbgen.PrLabel) error {
	act := actor.FromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{Actor: act, Owner: owner, Repo: repo, PrNumber: prNumber}); err != nil {
		return err
	}
	for _, l := range labels {
		if err := q.InsertPRLabel(ctx, dbgen.InsertPRLabelParams{
			Actor:    act,
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
	act := actor.FromContext(ctx)
	return s.q.ListPRLabels(ctx, dbgen.ListPRLabelsParams{Actor: act, Owner: owner, Repo: repo, PrNumber: prNumber})
}

// ---- PR Files ----

func (s *Store) SetPRFiles(ctx context.Context, owner, repo string, prNumber int64, files []dbgen.PrFile) error {
	act := actor.FromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := q.DeletePRFiles(ctx, dbgen.DeletePRFilesParams{Actor: act, Owner: owner, Repo: repo, PrNumber: prNumber}); err != nil {
		return err
	}
	for _, f := range files {
		if err := q.InsertPRFile(ctx, dbgen.InsertPRFileParams{
			Actor:     act,
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
	act := actor.FromContext(ctx)
	return s.q.ListPRFiles(ctx, dbgen.ListPRFilesParams{Actor: act, Owner: owner, Repo: repo, PrNumber: prNumber})
}

// ---- Branch Comparisons ----

func (s *Store) UpsertComparison(ctx context.Context, c dbgen.BranchComparison) error {
	act := actor.FromContext(ctx)
	return s.q.UpsertBranchComparison(ctx, dbgen.UpsertBranchComparisonParams{
		Actor:    act,
		Owner:    c.Owner,
		Repo:     c.Repo,
		BaseRef:  c.BaseRef,
		HeadRef:  c.HeadRef,
		AheadBy:  c.AheadBy,
		BehindBy: c.BehindBy,
	})
}

func (s *Store) GetComparison(ctx context.Context, owner, repo, base, head string) (dbgen.BranchComparison, error) {
	act := actor.FromContext(ctx)
	return s.q.GetBranchComparison(ctx, dbgen.GetBranchComparisonParams{
		Actor:   act,
		Owner:   owner,
		Repo:    repo,
		BaseRef: base,
		HeadRef: head,
	})
}

// ---- Cross-Actor Operations (for webhooks) ----

// ActorsForRepo returns all distinct actors that have cached data for the given repo.
func (s *Store) ActorsForRepo(ctx context.Context, owner, repo string) ([]string, error) {
	return s.q.ListActorsForRepo(ctx, dbgen.ListActorsForRepoParams{
		Owner: owner,
		Name:  repo,
	})
}

// UpsertPRForActors upserts a PR and its labels for every given actor.
func (s *Store) UpsertPRForActors(ctx context.Context, actors []string, pr dbgen.PullRequest, labels []dbgen.PrLabel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	for _, act := range actors {
		if err := q.UpsertPullRequest(ctx, prToParams(act, pr)); err != nil {
			return err
		}
		if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{
			Actor: act, Owner: pr.Owner, Repo: pr.Repo, PrNumber: pr.Number,
		}); err != nil {
			return err
		}
		for _, l := range labels {
			if err := q.InsertPRLabel(ctx, dbgen.InsertPRLabelParams{
				Actor:    act,
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
	return tx.Commit()
}

// DeletePRForActors deletes a PR and its labels for every given actor.
func (s *Store) DeletePRForActors(ctx context.Context, actors []string, owner, repo string, prNumber int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	for _, act := range actors {
		if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{
			Actor: act, Owner: owner, Repo: repo, PrNumber: prNumber,
		}); err != nil {
			return err
		}
		if err := q.DeletePullRequest(ctx, dbgen.DeletePullRequestParams{
			Actor: act, Owner: owner, Repo: repo, Number: prNumber,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- Helpers ----

func repoToParams(act string, r dbgen.Repo) dbgen.UpsertRepoParams {
	return dbgen.UpsertRepoParams{
		Actor:               act,
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

func prToParams(act string, pr dbgen.PullRequest) dbgen.UpsertPullRequestParams {
	return dbgen.UpsertPullRequestParams{
		Actor:              act,
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
