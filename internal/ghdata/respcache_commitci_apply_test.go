package ghdata

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ciSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func ptr(s string) *string { return &s }

func combinedDoc(t *testing.T, state string, items ...storedCommitStatusItem) string {
	t.Helper()
	b, err := MarshalCacheDoc(storedCombinedStatus{
		State: state, SHA: ciSHA, TotalCount: int64(len(items)), Statuses: items,
	})
	require.NoError(t, err)
	return string(b)
}

func statusItem(context, state, created string) storedCommitStatusItem {
	return storedCommitStatusItem{
		Context: context, State: state, Description: nil,
		CreatedAt: created, UpdatedAt: created,
	}
}

func update(context, state, created string) CommitStatusUpdate {
	return CommitStatusUpdate{
		SHA: ciSHA, Context: context, State: state,
		CreatedAt: created, UpdatedAt: created,
	}
}

// A re-posted context is a NEW status upstream, and the combined status holds
// the latest one per context oldest-first -- so the entry LEAVES its old
// position and lands at the end, with the rollup recomputed.
func TestPatchCombinedStatus_ReplacedContextMovesToTheEnd(t *testing.T) {
	doc := combinedDoc(t, "pending",
		statusItem("ci/build", "success", "2026-07-01T10:00:00Z"),
		statusItem("lint", "pending", "2026-07-01T10:01:00Z"),
	)
	got, ok := patchCombinedStatus(doc, 30, 1, update("lint", "success", "2026-07-01T10:05:00Z"))
	require.True(t, ok)

	var d storedCombinedStatus
	require.NoError(t, json.Unmarshal([]byte(got), &d))
	assert.Equal(t, []string{"ci/build", "lint"}, []string{d.Statuses[0].Context, d.Statuses[1].Context})
	assert.Equal(t, "2026-07-01T10:05:00Z", d.Statuses[1].CreatedAt)
	assert.Equal(t, int64(2), d.TotalCount, "a replaced context does not grow the count")
	assert.Equal(t, "success", d.State, "the last pending context finished")
}

func TestPatchCombinedStatus_NewContextAppends(t *testing.T) {
	doc := combinedDoc(t, "success", statusItem("ci/build", "success", "2026-07-01T10:00:00Z"))
	got, ok := patchCombinedStatus(doc, 30, 1, update("lint", "failure", "2026-07-01T10:05:00Z"))
	require.True(t, ok)

	var d storedCombinedStatus
	require.NoError(t, json.Unmarshal([]byte(got), &d))
	assert.Equal(t, int64(2), d.TotalCount)
	assert.Equal(t, "lint", d.Statuses[1].Context)
	assert.Equal(t, "failure", d.State, "one failing context fails the rollup")
}

// An `error` state rolls up as failure, and failure outranks a pending
// context -- GitHub's documented combined state, which the rewrite has to
// reproduce exactly or a hit and a miss disagree.
func TestPatchCombinedStatus_Rollup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []storedCommitStatusItem
		want  string
	}{
		{"no contexts", nil, "pending"},
		{"all success", []storedCommitStatusItem{statusItem("a", "success", "2026-07-01T09:00:00Z")}, "success"},
		{"any pending", []storedCommitStatusItem{
			statusItem("a", "success", "2026-07-01T09:00:00Z"),
			statusItem("b", "pending", "2026-07-01T09:00:00Z"),
		}, "pending"},
		{"error outranks pending", []storedCommitStatusItem{
			statusItem("a", "error", "2026-07-01T09:00:00Z"),
			statusItem("b", "pending", "2026-07-01T09:00:00Z"),
		}, "failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, combinedStatusRollup(tc.items))
		})
	}
}

// Everything the rewrite cannot PROVE it would get right refuses, and the
// caller drops the row instead -- which is what happened to every one of these
// before the rewrite existed, so refusing costs nothing.
func TestPatchCombinedStatus_RefusesWhatItCannotProve(t *testing.T) {
	full := combinedDoc(t, "success",
		statusItem("ci/build", "success", "2026-07-01T10:00:00Z"),
		statusItem("lint", "success", "2026-07-01T10:01:00Z"),
	)

	t.Run("a page other than the first", func(t *testing.T) {
		_, ok := patchCombinedStatus(full, 30, 2, update("new", "success", "2026-07-01T11:00:00Z"))
		assert.False(t, ok)
	})

	t.Run("a page that does not hold every context", func(t *testing.T) {
		var d storedCombinedStatus
		require.NoError(t, json.Unmarshal([]byte(full), &d))
		d.TotalCount = 9 // later pages carry contexts this rewrite cannot see
		partial, err := MarshalCacheDoc(d)
		require.NoError(t, err)
		_, ok := patchCombinedStatus(string(partial), 30, 1, update("new", "success", "2026-07-01T11:00:00Z"))
		assert.False(t, ok)
	})

	t.Run("a status older than one already stored", func(t *testing.T) {
		_, ok := patchCombinedStatus(full, 30, 1, update("new", "success", "2026-07-01T09:00:00Z"))
		assert.False(t, ok, "where an older entry belongs in the ordering is unknown")
	})

	t.Run("a new context with no room on the page", func(t *testing.T) {
		_, ok := patchCombinedStatus(full, 2, 1, update("new", "success", "2026-07-01T11:00:00Z"))
		assert.False(t, ok)
		_, ok = patchCombinedStatus(full, 2, 1, update("lint", "failure", "2026-07-01T11:00:00Z"))
		assert.True(t, ok, "replacing a context does not grow the page")
	})

	t.Run("a document about another commit", func(t *testing.T) {
		other := update("lint", "success", "2026-07-01T11:00:00Z")
		other.SHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		_, ok := patchCombinedStatus(full, 30, 1, other)
		assert.False(t, ok)
	})

	t.Run("an unreadable document", func(t *testing.T) {
		_, ok := patchCombinedStatus(`{`, 30, 1, update("lint", "success", "2026-07-01T11:00:00Z"))
		assert.False(t, ok)
	})
}

// The raw list is append-only HISTORY, newest-first: a re-posted context keeps
// its older entries, exactly as upstream keeps them.
func TestPatchStatusesList_PrependsAndKeepsHistory(t *testing.T) {
	doc, err := MarshalCacheDoc([]storedStatusListItem{
		{Context: "ci/build", State: "pending", CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-01T10:00:00Z"},
	})
	require.NoError(t, err)

	up := update("ci/build", "success", "2026-07-01T10:05:00Z")
	up.TargetURL = ptr("https://rbm.example.com/b")
	got, ok := patchStatusesList(string(doc), 30, 1, up)
	require.True(t, ok)

	var items []storedStatusListItem
	require.NoError(t, json.Unmarshal([]byte(got), &items))
	require.Len(t, items, 2, "the older status stays: the list is history, not the latest per context")
	assert.Equal(t, "success", items[0].State, "newest first")
	assert.Equal(t, "pending", items[1].State)
	require.NotNil(t, items[0].TargetURL)
	assert.Equal(t, "https://rbm.example.com/b", *items[0].TargetURL)
}

func TestPatchStatusesList_PageOverflowDropsTheOldestEntry(t *testing.T) {
	doc, err := MarshalCacheDoc([]storedStatusListItem{
		{Context: "b", State: "success", CreatedAt: "2026-07-01T10:01:00Z", UpdatedAt: "2026-07-01T10:01:00Z"},
		{Context: "a", State: "success", CreatedAt: "2026-07-01T10:00:00Z", UpdatedAt: "2026-07-01T10:00:00Z"},
	})
	require.NoError(t, err)

	got, ok := patchStatusesList(string(doc), 2, 1, update("c", "success", "2026-07-01T10:05:00Z"))
	require.True(t, ok)

	var items []storedStatusListItem
	require.NoError(t, json.Unmarshal([]byte(got), &items))
	require.Len(t, items, 2, "a full page keeps its size; the oldest entry is where page 2 begins")
	assert.Equal(t, []string{"c", "b"}, []string{items[0].Context, items[1].Context})
}

// Equal event times still apply upstream (GitHub's clocks are second-granular
// and the delivery gate refuses on strictly-older only), so a REDELIVERY can
// reach this. Prepending it again would invent a status that never existed.
func TestPatchStatusesList_RedeliveryIsNotDuplicated(t *testing.T) {
	doc, err := MarshalCacheDoc([]storedStatusListItem{
		{Context: "ci/build", State: "success", CreatedAt: "2026-07-01T10:05:00Z", UpdatedAt: "2026-07-01T10:05:00Z"},
	})
	require.NoError(t, err)

	got, ok := patchStatusesList(string(doc), 30, 1, update("ci/build", "success", "2026-07-01T10:05:00Z"))
	require.True(t, ok)
	assert.Equal(t, string(doc), got, "an already-absorbed status leaves the document alone")
}

func TestPatchStatusesList_RefusesAnOlderStatusAndLaterPages(t *testing.T) {
	doc, err := MarshalCacheDoc([]storedStatusListItem{
		{Context: "a", State: "success", CreatedAt: "2026-07-01T10:05:00Z", UpdatedAt: "2026-07-01T10:05:00Z"},
	})
	require.NoError(t, err)

	_, ok := patchStatusesList(string(doc), 30, 1, update("b", "success", "2026-07-01T10:00:00Z"))
	assert.False(t, ok, "an older status does not belong at the head")
	_, ok = patchStatusesList(string(doc), 30, 2, update("b", "success", "2026-07-01T11:00:00Z"))
	assert.False(t, ok, "only the first page has a known head")
}

// SettleCommitCIFromStatus decides which ROWS it may touch from what the
// payload can identify, not from the spellings the caller names: a combined
// status states the sha it resolved to, so a branch-form row pointing at
// another commit was not moved by this status and must keep serving.
func TestSettleCommitCIFromStatus_LeavesRowsAboutOtherCommits(t *testing.T) {
	s := testStore(t)
	ctx, now := context.Background(), time.Now()
	const otherSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	here := combinedDoc(t, "pending", statusItem("lint", "pending", "2026-07-01T10:00:00Z"))
	other, err := MarshalCacheDoc(storedCombinedStatus{
		State: "success", SHA: otherSHA, Statuses: []storedCommitStatusItem{},
	})
	require.NoError(t, err)
	elsewhere := string(other)
	for _, seed := range []struct{ ref, doc string }{
		{"main", here},         // the branch points at the commit this status is on
		{ciSHA, here},          // the sha spelling of the same answer
		{"release", elsewhere}, // a branch pointing somewhere else entirely
	} {
		require.NoError(t, s.PutCachedCommitCI(ctx, CachedCommitCI{
			Owner: "org1", Repo: "repo1", Ref: seed.ref, Kind: CommitCIKindStatus, Doc: seed.doc,
		}, 30, 1, now, time.Hour))
	}

	require.NoError(t, s.SettleCommitCIFromStatus(ctx, "org1", "repo1",
		[]string{"main", "heads/main", "refs/heads/main", ciSHA},
		update("lint", "success", "2026-07-01T10:05:00Z"), now, CommitCICacheTTL))

	for _, ref := range []string{"main", ciSHA} {
		got, ok, err := s.GetCachedCommitCI(ctx, "org1", "repo1", ref, CommitCIKindStatus, 30, 1, now)
		require.NoError(t, err)
		require.True(t, ok, "%s: rewritten, not dropped", ref)
		assert.Contains(t, got.Doc, `"state":"success"`)
	}
	got, ok, err := s.GetCachedCommitCI(ctx, "org1", "repo1", "release", CommitCIKindStatus, 30, 1, now)
	require.NoError(t, err)
	require.True(t, ok, "a branch pointing at another commit was not moved by this status")
	assert.Equal(t, elsewhere, got.Doc)
}
