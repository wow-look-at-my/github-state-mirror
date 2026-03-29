package ghdata

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func TestUpsertAndGetUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	user := dbgen.User{Login: "octocat", AvatarUrl: "http://avatar", Url: "http://url"}
	if err := s.UpsertUser(ctx, user); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	got, err := s.GetFirstUser(ctx)
	if err != nil {
		t.Fatalf("GetFirstUser: %v", err)
	}
	if got.Login != "octocat" {
		t.Errorf("login = %q, want %q", got.Login, "octocat")
	}
	if got.AvatarUrl != "http://avatar" {
		t.Errorf("avatar_url = %q, want %q", got.AvatarUrl, "http://avatar")
	}
}

func TestSetUserOrgs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertUser(ctx, dbgen.User{Login: "octocat", AvatarUrl: "a", Url: "u"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	orgs := []dbgen.Org{
		{Login: "org1", AvatarUrl: sql.NullString{String: "a1", Valid: true}, Url: sql.NullString{String: "u1", Valid: true}},
		{Login: "org2", AvatarUrl: sql.NullString{String: "a2", Valid: true}, Url: sql.NullString{String: "u2", Valid: true}},
	}
	if err := s.SetUserOrgs(ctx, "octocat", orgs); err != nil {
		t.Fatalf("SetUserOrgs: %v", err)
	}

	got, err := s.ListUserOrgs(ctx, "octocat")
	if err != nil {
		t.Fatalf("ListUserOrgs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	// Replace with different orgs.
	newOrgs := []dbgen.Org{
		{Login: "org3", AvatarUrl: sql.NullString{String: "a3", Valid: true}, Url: sql.NullString{String: "u3", Valid: true}},
	}
	if err := s.SetUserOrgs(ctx, "octocat", newOrgs); err != nil {
		t.Fatalf("SetUserOrgs 2: %v", err)
	}

	got, err = s.ListUserOrgs(ctx, "octocat")
	if err != nil {
		t.Fatalf("ListUserOrgs 2: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Login != "org3" {
		t.Errorf("login = %q, want %q", got[0].Login, "org3")
	}
}

func TestSetOrgRepos(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	repos := []dbgen.Repo{
		{Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "u1"},
		{Owner: "org1", Name: "repo2", NameWithOwner: "org1/repo2", Url: "u2"},
	}
	if err := s.SetOrgRepos(ctx, "org1", repos); err != nil {
		t.Fatalf("SetOrgRepos: %v", err)
	}

	got, err := s.ListReposByOwner(ctx, "org1")
	if err != nil {
		t.Fatalf("ListReposByOwner: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	// Replace — one fewer repo.
	if err := s.SetOrgRepos(ctx, "org1", repos[:1]); err != nil {
		t.Fatalf("SetOrgRepos 2: %v", err)
	}
	got, _ = s.ListReposByOwner(ctx, "org1")
	if len(got) != 1 {
		t.Errorf("len = %d, want 1 after replace", len(got))
	}
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
	if err := s.SetRepoPRs(ctx, "org1", "repo1", prs, labels); err != nil {
		t.Fatalf("SetRepoPRs: %v", err)
	}

	got, err := s.ListOpenPRsByRepo(ctx, "org1", "repo1")
	if err != nil {
		t.Fatalf("ListOpenPRsByRepo: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	gotLabels, err := s.ListPRLabels(ctx, "org1", "repo1", 1)
	if err != nil {
		t.Fatalf("ListPRLabels: %v", err)
	}
	if len(gotLabels) != 1 {
		t.Fatalf("labels len = %d, want 1", len(gotLabels))
	}
	if gotLabels[0].Name != "bug" {
		t.Errorf("label name = %q, want %q", gotLabels[0].Name, "bug")
	}
}

func TestSetPRFiles(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	files := []dbgen.PrFile{
		{Owner: "org1", Repo: "repo1", PrNumber: 1, Path: "main.go", Additions: 10, Deletions: 5},
		{Owner: "org1", Repo: "repo1", PrNumber: 1, Path: "test.go", Additions: 20, Deletions: 0},
	}
	if err := s.SetPRFiles(ctx, "org1", "repo1", 1, files); err != nil {
		t.Fatalf("SetPRFiles: %v", err)
	}

	got, err := s.ListPRFiles(ctx, "org1", "repo1", 1)
	if err != nil {
		t.Fatalf("ListPRFiles: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	// Replace with fewer files.
	if err := s.SetPRFiles(ctx, "org1", "repo1", 1, files[:1]); err != nil {
		t.Fatalf("SetPRFiles 2: %v", err)
	}
	got, _ = s.ListPRFiles(ctx, "org1", "repo1", 1)
	if len(got) != 1 {
		t.Errorf("len = %d, want 1 after replace", len(got))
	}
}

func TestUpsertAndGetComparison(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	comp := dbgen.BranchComparison{
		Owner: "org1", Repo: "repo1", BaseRef: "main", HeadRef: "feature", AheadBy: 3, BehindBy: 1,
	}
	if err := s.UpsertComparison(ctx, comp); err != nil {
		t.Fatalf("UpsertComparison: %v", err)
	}

	got, err := s.GetComparison(ctx, "org1", "repo1", "main", "feature")
	if err != nil {
		t.Fatalf("GetComparison: %v", err)
	}
	if got.AheadBy != 3 || got.BehindBy != 1 {
		t.Errorf("ahead=%d behind=%d, want 3/1", got.AheadBy, got.BehindBy)
	}
}
