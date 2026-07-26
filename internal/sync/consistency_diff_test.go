package sync

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// TestRepoFieldDiffs_Table covers the new repo field comparisons.
func TestRepoFieldDiffs_Table(t *testing.T) {
	// checkStart is well after every base timestamp, so the pre-existing cases
	// all read as strictly-before-the-check drift; the raced_* cases place
	// GitHub's value at/after it.
	checkStart := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	base := func() dbgen.Repo {
		return dbgen.Repo{
			Owner: "o", Name: "r", NameWithOwner: "o/r", Url: "u",
			PushedAt: sql.NullString{String: "2026-01-01T00:00:00Z", Valid: true},
		}
	}
	pushed := func(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

	cases := []struct {
		name      string
		mutate    func(c, g *dbgen.Repo)
		vis       map[string]ghclient.OwnerRepoVisibility
		wantField string
		wantIssue string
	}{
		{
			name:   "identical yields nothing",
			mutate: func(c, g *dbgen.Repo) {},
			vis:    map[string]ghclient.OwnerRepoVisibility{"r": {Visibility: ""}},
		},
		{
			name:      "cached public github private is a leak",
			mutate:    func(c, g *dbgen.Repo) { c.Visibility = "public" },
			vis:       map[string]ghclient.OwnerRepoVisibility{"r": {Visibility: "private"}},
			wantField: "visibility", wantIssue: "visibility_leak",
		},
		{
			name:      "cached public github internal is a leak",
			mutate:    func(c, g *dbgen.Repo) { c.Visibility = "public" },
			vis:       map[string]ghclient.OwnerRepoVisibility{"r": {Visibility: "internal"}},
			wantField: "visibility", wantIssue: "visibility_leak",
		},
		{
			name:      "cached private github public is a plain mismatch",
			mutate:    func(c, g *dbgen.Repo) { c.Visibility = "private" },
			vis:       map[string]ghclient.OwnerRepoVisibility{"r": {Visibility: "public"}},
			wantField: "visibility", wantIssue: "field_mismatch",
		},
		{
			name:      "cached unknown is informational",
			mutate:    func(c, g *dbgen.Repo) { c.Visibility = "" },
			vis:       map[string]ghclient.OwnerRepoVisibility{"r": {Visibility: "public"}},
			wantField: "visibility", wantIssue: "visibility_unknown",
		},
		{
			name:   "matching visibility yields nothing",
			mutate: func(c, g *dbgen.Repo) { c.Visibility = "private" },
			vis:    map[string]ghclient.OwnerRepoVisibility{"r": {Visibility: "private"}},
		},
		{
			name:   "no visibility data yields no visibility diff",
			mutate: func(c, g *dbgen.Repo) { c.Visibility = "public" },
			vis:    nil,
		},
		{
			name:      "pushed_at lag beyond tolerance flags",
			mutate:    func(c, g *dbgen.Repo) { g.PushedAt = pushed("2026-01-01T00:10:00Z") },
			wantField: "pushed_at", wantIssue: "field_mismatch",
		},
		{
			name:   "pushed_at lag inside tolerance is race noise",
			mutate: func(c, g *dbgen.Repo) { g.PushedAt = pushed("2026-01-01T00:04:00Z") },
		},
		{
			name:   "cached pushed_at NEWER than github is the fetch racing a push",
			mutate: func(c, g *dbgen.Repo) { c.PushedAt = pushed("2026-01-01T01:00:00Z") },
		},
		{
			name:      "cached pushed_at missing while github has one flags",
			mutate:    func(c, g *dbgen.Repo) { c.PushedAt = sql.NullString{} },
			wantField: "pushed_at", wantIssue: "field_mismatch",
		},
		{
			name:      "pushed_at one second before check start stays drift",
			mutate:    func(c, g *dbgen.Repo) { g.PushedAt = pushed("2026-01-31T23:59:59Z") },
			wantField: "pushed_at", wantIssue: "field_mismatch",
		},
		{
			name:      "pushed_at exactly at check start is raced (>= boundary)",
			mutate:    func(c, g *dbgen.Repo) { g.PushedAt = pushed("2026-02-01T00:00:00Z") },
			wantField: "pushed_at", wantIssue: "raced_during_check",
		},
		{
			name:      "pushed_at after check start is raced",
			mutate:    func(c, g *dbgen.Repo) { g.PushedAt = pushed("2026-02-01T00:07:00Z") },
			wantField: "pushed_at", wantIssue: "raced_during_check",
		},
		{
			name:      "archive drift via the visibility map",
			mutate:    func(c, g *dbgen.Repo) {},
			vis:       map[string]ghclient.OwnerRepoVisibility{"r": {Visibility: "public", Archived: true}},
			wantField: "is_archived", wantIssue: "field_mismatch",
		},
		{
			name:      "cached archived while live in org data flags",
			mutate:    func(c, g *dbgen.Repo) { c.IsArchived = 1 },
			vis:       map[string]ghclient.OwnerRepoVisibility{"r": {Visibility: ""}},
			wantField: "is_archived", wantIssue: "field_mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, g := base(), base()
			// Keep visibility consistent by default so only the mutation diffs.
			if v, ok := tc.vis["r"]; ok && v.Visibility != "" {
				c.Visibility = v.Visibility
			}
			tc.mutate(&c, &g)
			diffs := repoFieldDiffs("o", "r", c, g, tc.vis, checkStart)
			if tc.wantField == "" {
				assert.Empty(t, diffs)
				return
			}
			require.Len(t, diffs, 1)
			assert.Equal(t, tc.wantField, diffs[0].Field)
			assert.Equal(t, tc.wantIssue, diffs[0].Issue)
		})
	}
}

// TestDefaultBranchDiff_CarriesGitHubValue: a default_branch mismatch entry
// must ALWAYS name both sides. GraphQL's defaultBranchRef is null for a repo
// with no commits (REST still reports the CONFIGURED default_branch name,
// which is what the cache holds), and the old shared add() rendered that as ""
// -- silently dropped from the report JSON by omitempty, so the 2026-07-20
// entries carried no github value at all.
func TestDefaultBranchDiff_CarriesGitHubValue(t *testing.T) {
	checkStart := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	base := dbgen.Repo{
		Owner: "o", Name: "r", NameWithOwner: "o/r", Url: "u",
		DefaultBranch: sql.NullString{String: "main", Valid: true},
	}

	// A real name difference carries GitHub's value verbatim.
	c, g := base, base
	g.DefaultBranch = sql.NullString{String: "master", Valid: true}
	diffs := repoFieldDiffs("o", "r", c, g, nil, checkStart)
	require.Len(t, diffs, 1)
	assert.Equal(t, "default_branch", diffs[0].Field)
	assert.Equal(t, "field_mismatch", diffs[0].Issue)
	assert.Equal(t, "main", diffs[0].Cached)
	assert.Equal(t, "master", diffs[0].GitHub)
	assert.Empty(t, diffs[0].Note)

	// GitHub reported NO default branch ref: the github side is an explicit
	// marker (surviving the JSON omitempty), plus a note explaining why.
	c, g = base, base
	g.DefaultBranch = sql.NullString{}
	diffs = repoFieldDiffs("o", "r", c, g, nil, checkStart)
	require.Len(t, diffs, 1)
	assert.Equal(t, "default_branch", diffs[0].Field)
	assert.Equal(t, "main", diffs[0].Cached)
	assert.Equal(t, "(none)", diffs[0].GitHub)
	assert.Contains(t, diffs[0].Note, "no default branch ref")

	// The reverse gap is explicit too: a cached row with none recorded.
	c, g = base, base
	c.DefaultBranch = sql.NullString{}
	diffs = repoFieldDiffs("o", "r", c, g, nil, checkStart)
	require.Len(t, diffs, 1)
	assert.Equal(t, "(none)", diffs[0].Cached)
	assert.Equal(t, "main", diffs[0].GitHub)
}

// TestPRFieldDiffs_RacedHeadMove: a head_ref_oid difference whose PR was
// updated at/after the check's start (the in-flight-movement proof the
// owner-query snapshot carries) is raced_during_check; strictly before -- or
// unprovable -- stays field_mismatch.
func TestPRFieldDiffs_RacedHeadMove(t *testing.T) {
	checkStart := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	base := func() dbgen.PullRequest {
		return dbgen.PullRequest{
			Owner: "o", Repo: "r", Number: 1, Title: "T", Url: "u", State: "OPEN",
			CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-15T00:00:00Z",
			HeadRefOid: sql.NullString{String: "oldsha", Valid: true},
		}
	}
	cases := []struct {
		name      string
		updatedAt string
		wantIssue string
	}{
		{"github updated_at strictly before check start stays drift", "2026-01-31T23:59:59Z", "field_mismatch"},
		{"github updated_at exactly at check start is raced (>= boundary)", "2026-02-01T00:00:00Z", "raced_during_check"},
		{"github updated_at after check start is raced", "2026-02-01T00:03:00Z", "raced_during_check"},
		{"unparseable github updated_at cannot prove a race", "not-a-time", "field_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, g := base(), base()
			g.HeadRefOid = sql.NullString{String: "newsha", Valid: true}
			g.UpdatedAt = tc.updatedAt
			diffs := prFieldDiffs("o/r", 1, c, g, checkStart)
			require.Len(t, diffs, 1)
			assert.Equal(t, "head_ref_oid", diffs[0].Field)
			assert.Equal(t, tc.wantIssue, diffs[0].Issue)
			assert.Equal(t, "oldsha", diffs[0].Cached)
			assert.Equal(t, "newsha", diffs[0].GitHub)
		})
	}
}

// TestConsistencyChecker_RacedDuringCheck: end to end, a repo pushed -- and a
// PR head moved -- WHILE the check runs (GitHub-side timestamps at/after the
// run's check_started_at) classify as informational raced_during_check with
// their own summary tally, never as field_mismatch drift.
func TestConsistencyChecker_RacedDuringCheck(t *testing.T) {
	future := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	pr := livePR(1, "newsha", "", "")
	pr["updatedAt"] = future
	repo := liveRepo("org1", "repo1", "", []map[string]any{pr})
	repo["pushedAt"] = future
	srv := consistencyFakeGitHub(t, map[string]fakeOwner{
		"org1": {
			repos: []map[string]any{repo},
			vis:   []map[string]any{visNode("repo1", "PUBLIC", false)},
		},
	})
	checker, store, _ := newCheckerTest(t, srv.URL)
	ctx := context.Background()

	// Cached truth matches the live state everywhere EXCEPT the raced fields,
	// so the report isolates the two classifications under test.
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "org1", Name: "repo1", NameWithOwner: "org1/repo1", Url: "https://github.com/org1/repo1",
		Visibility:    "public",
		PushedAt:      sql.NullString{String: "2024-01-01T00:00:00Z", Valid: true},
		DefaultBranch: sql.NullString{String: "main", Valid: true},
	}))
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "org1", Repo: "repo1", Number: 1, Title: "Live title", Url: "https://github.com/org1/repo1/pull/1",
		State: "OPEN", CreatedAt: "2024-01-01", UpdatedAt: "2024-01-02",
		HeadRefOid:         sql.NullString{String: "oldsha", Valid: true},
		HeadRefName:        sql.NullString{String: "feature", Valid: true},
		BaseRefName:        sql.NullString{String: "main", Valid: true},
		ReviewRequestCount: sql.NullInt64{Int64: 1, Valid: true},
	}, time.Now()))
	require.NoError(t, store.SetPRLabels(ctx, "org1", "repo1", 1, []dbgen.PrLabel{
		{Owner: "org1", Repo: "repo1", PrNumber: 1, Name: "bug", Color: "d73a4a"},
	}))

	rep, err := checker.Check(ctx, "org1")
	require.NoError(t, err)
	assert.NotEmpty(t, rep.CheckStartedAt, "the report must carry the classification anchor")

	if d := findDiscrepancy(rep, "org1/repo1", "pushed_at", "raced_during_check"); assert.NotNil(t, d, "in-flight push is informational") {
		assert.Equal(t, future, d.GitHub)
		assert.Contains(t, d.Note, "while the check ran")
		assert.Contains(t, d.Fix, "re-run to confirm")
	}
	if d := findDiscrepancy(rep, "org1/repo1", "head_ref_oid", "raced_during_check"); assert.NotNil(t, d, "in-flight head move is informational") {
		assert.Equal(t, "oldsha", d.Cached)
		assert.Equal(t, "newsha", d.GitHub)
		assert.Contains(t, d.Fix, "re-run to confirm")
	}
	assert.Nil(t, findDiscrepancy(rep, "org1/repo1", "pushed_at", "field_mismatch"), "a raced push must not double-report as drift")
	assert.Nil(t, findDiscrepancy(rep, "org1/repo1", "head_ref_oid", "field_mismatch"), "a raced head move must not double-report as drift")
	assert.Equal(t, 2, rep.Summary.RacedDuringCheck, "raced entries get their own tally")
	assert.Zero(t, rep.Summary.FieldMismatches, "raced entries never count as field mismatches")
}
