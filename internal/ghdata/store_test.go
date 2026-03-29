package ghdata

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)

	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func TestUpsertAndGetUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	user := dbgen.User{Login: "octocat", AvatarUrl: "http://avatar", Url: "http://url"}
	require.NoError(t, s.UpsertUser(ctx, user))

	got, err := s.GetFirstUser(ctx)
	require.Nil(t, err)

	assert.Equal(t, "octocat", got.Login)

	assert.Equal(t, "http://avatar", got.AvatarUrl)

}

func TestSetUserOrgs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertUser(ctx, dbgen.User{Login: "octocat", AvatarUrl: "a", Url: "u"}))

	orgs := []dbgen.Org{
		{Login: "org1", AvatarUrl: sql.NullString{String: "a1", Valid: true}, Url: sql.NullString{String: "u1", Valid: true}},
		{Login: "org2", AvatarUrl: sql.NullString{String: "a2", Valid: true}, Url: sql.NullString{String: "u2", Valid: true}},
	}
	require.NoError(t, s.SetUserOrgs(ctx, "octocat", orgs))

	got, err := s.ListUserOrgs(ctx, "octocat")
	require.Nil(t, err)

	require.Equal(t, 2, len(got))

	// Replace with different orgs.
	newOrgs := []dbgen.Org{
		{Login: "org3", AvatarUrl: sql.NullString{String: "a3", Valid: true}, Url: sql.NullString{String: "u3", Valid: true}},
	}
	require.NoError(t, s.SetUserOrgs(ctx, "octocat", newOrgs))

	got, err = s.ListUserOrgs(ctx, "octocat")
	require.Nil(t, err)

	require.Equal(t, 1, len(got))

	assert.Equal(t, "org3", got[0].Login)

}

func TestSetOrgRepos(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	repos := []dbgen.Repo{
		{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u1"},
		{Owner: "org1", Name: "repo2", NameWithOwner: "org1/repo2", Url: "u2"},
	}
	require.NoError(t, s.SetOrgRepos(ctx, "org1", repos))

	got, err := s.ListReposByOwner(ctx, "org1")
	require.Nil(t, err)

	require.Equal(t, 2, len(got))

	// Replace — one fewer repo.
	require.NoError(t, s.SetOrgRepos(ctx, "org1", repos[:1]))

	got, _ = s.ListReposByOwner(ctx, "org1")
	assert.Equal(t, 1, len(got))

}

func TestSetRepoPRs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	prs := []dbgen.PullRequest{
		{Owner: "org1", Repo: "repo1", Number: 1, Title: "PR 1", Url: "u1", State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-01"},
		{Owner: "org1", Repo: "repo1", Number: 2, Title: "PR 2", Url: "u2", State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-01"},
	}
	labels := map[int64][]dbgen.PrLabel{
		1: {{Owner: "org1", Repo: "repo1", PrNumber: 1, Name: "bug", Color: "red"}},
	}
	require.NoError(t, s.SetRepoPRs(ctx, "org1", "repo1", prs, labels))

	got, err := s.ListOpenPRsByRepo(ctx, "org1", "repo1")
	require.Nil(t, err)

	require.Equal(t, 2, len(got))

	gotLabels, err := s.ListPRLabels(ctx, "org1", "repo1", 1)
	require.Nil(t, err)

	require.Equal(t, 1, len(gotLabels))

	assert.Equal(t, "bug", gotLabels[0].Name)

}

func TestSetPRFiles(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	files := []dbgen.PrFile{
		{Owner: "org1", Repo: "repo1", PrNumber: 1, Path: "main.go", Additions: 10, Deletions: 5},
		{Owner: "org1", Repo: "repo1", PrNumber: 1, Path: "test.go", Additions: 20, Deletions: 0},
	}
	require.NoError(t, s.SetPRFiles(ctx, "org1", "repo1", 1, files))

	got, err := s.ListPRFiles(ctx, "org1", "repo1", 1)
	require.Nil(t, err)

	require.Equal(t, 2, len(got))

	// Replace with fewer files.
	require.NoError(t, s.SetPRFiles(ctx, "org1", "repo1", 1, files[:1]))

	got, _ = s.ListPRFiles(ctx, "org1", "repo1", 1)
	assert.Equal(t, 1, len(got))

}

func TestUpsertAndGetComparison(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	comp := dbgen.BranchComparison{
		Owner:	"org1", Repo: "repo1", BaseRef: "main", HeadRef: "feature", AheadBy: 3, BehindBy: 1,
	}
	require.NoError(t, s.UpsertComparison(ctx, comp))

	got, err := s.GetComparison(ctx, "org1", "repo1", "main", "feature")
	require.Nil(t, err)

	assert.False(t, got.AheadBy != 3 || got.BehindBy != 1)

}
