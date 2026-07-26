package notify

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "subscriptions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

func testSecret() string { return strings.Repeat("k", 32) }

func mustCreate(t *testing.T, st *Store, principal string, in NewSubscription) Subscription {
	t.Helper()
	if in.URL == "" {
		in.URL = "https://example.com/hook"
	}
	if in.Secret == "" {
		in.Secret = testSecret()
	}
	sub, err := st.Create(context.Background(), principal, in, time.Now())
	require.NoError(t, err)
	return sub
}

func TestStoreCRUDRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sub := mustCreate(t, st, "user:1", NewSubscription{
		URL:    "https://example.com/hook",
		Secret: testSecret(),
		Repos:  []string{"My-Org", "my-org/repo1"},
		Events: []string{"push"},
	})
	assert.True(t, strings.HasPrefix(sub.ID, "sub_"), "id carries the sub_ prefix")
	assert.Len(t, sub.ID, len("sub_")+32, "sub_ + 16 random bytes hex")
	assert.True(t, sub.Active)
	assert.Equal(t, []string{"my-org", "my-org/repo1"}, sub.RepoFilters, "repo filters stored lowercased")
	_, err := time.Parse(time.RFC3339Nano, sub.CreatedAt)
	assert.NoError(t, err, "created_at is RFC3339Nano")

	got, err := st.Get(ctx, "user:1", sub.ID)
	require.NoError(t, err)
	assert.Equal(t, sub, got)

	list, err := st.List(ctx, "user:1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, sub.ID, list[0].ID)

	enabled, err := st.ListEnabled(ctx)
	require.NoError(t, err)
	assert.Len(t, enabled, 1)

	// Patch: change URL and events.
	newURL := "https://example.com/hook2"
	events := []string{"pull_request"}
	updated, err := st.Update(ctx, "user:1", sub.ID, Patch{URL: &newURL, Events: &events}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, newURL, updated.URL)
	assert.Equal(t, events, updated.EventFilters)
	assert.Equal(t, sub.RepoFilters, updated.RepoFilters, "unpatched fields survive")
	assert.Equal(t, sub.Secret, updated.Secret, "unpatched secret survives")

	// A patched value must fail validation like a created one.
	bad := "http://not-loopback.example.com/x"
	_, err = st.Update(ctx, "user:1", sub.ID, Patch{URL: &bad}, time.Now())
	var ve *ValidationError
	assert.ErrorAs(t, err, &ve)

	require.NoError(t, st.Delete(ctx, "user:1", sub.ID))
	_, err = st.Get(ctx, "user:1", sub.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, st.Delete(ctx, "user:1", sub.ID), ErrNotFound, "delete is not idempotent-silent: missing is 404")
}

func TestStorePrincipalScoping(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	mine := mustCreate(t, st, "user:1", NewSubscription{})
	theirs := mustCreate(t, st, "user:2", NewSubscription{})

	// A foreign principal's id is invisible on get/update/delete — same
	// ErrNotFound as a nonexistent id, no existence leak.
	_, err := st.Get(ctx, "user:2", mine.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	u := "https://example.com/other"
	_, err = st.Update(ctx, "user:2", mine.ID, Patch{URL: &u}, time.Now())
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, st.Delete(ctx, "user:2", mine.ID), ErrNotFound)

	// Each principal lists only their own.
	list1, err := st.List(ctx, "user:1")
	require.NoError(t, err)
	require.Len(t, list1, 1)
	assert.Equal(t, mine.ID, list1[0].ID)
	list2, err := st.List(ctx, "user:2")
	require.NoError(t, err)
	require.Len(t, list2, 1)
	assert.Equal(t, theirs.ID, list2[0].ID)

	// ListAll (operator view) sees both.
	all, err := st.ListAll(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestStorePerPrincipalCap(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < MaxPerPrincipal; i++ {
		mustCreate(t, st, "user:1", NewSubscription{})
	}
	_, err := st.Create(context.Background(), "user:1", NewSubscription{
		URL: "https://example.com/hook", Secret: testSecret(),
	}, time.Now())
	assert.ErrorIs(t, err, ErrLimitExceeded)

	// Another principal is unaffected by user:1's cap.
	mustCreate(t, st, "user:2", NewSubscription{})
}

func TestStoreCreateValidates(t *testing.T) {
	st := newTestStore(t)
	_, err := st.Create(context.Background(), "user:1", NewSubscription{
		URL: "http://example.com/hook", Secret: testSecret(),
	}, time.Now())
	var ve *ValidationError
	require.ErrorAs(t, err, &ve, "http to a non-loopback host is rejected")

	list, lerr := st.List(context.Background(), "user:1")
	require.NoError(t, lerr)
	assert.Empty(t, list, "a rejected create stores nothing")
}

func TestStoreActiveTrueResetsFailureState(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sub := mustCreate(t, st, "user:1", NewSubscription{})

	// Drive the subscription to auto-disable.
	var disabled bool
	for i := 0; i < 3; i++ {
		var err error
		_, disabled, err = st.RecordFailure(ctx, sub.ID, "http 500", 3, time.Now())
		require.NoError(t, err)
	}
	assert.True(t, disabled, "the third failure flips it")
	got, err := st.Get(ctx, "user:1", sub.ID)
	require.NoError(t, err)
	assert.False(t, got.Active)
	assert.Equal(t, int64(3), got.ConsecutiveFailures)
	assert.Contains(t, got.DisabledReason, "auto-disabled after 3 consecutive delivery failures")
	assert.Equal(t, "http 500", got.LastError)
	assert.NotEmpty(t, got.LastFailureAt)

	// PATCH active=true is the re-enable path: counter reset, reason cleared.
	on := true
	updated, err := st.Update(ctx, "user:1", sub.ID, Patch{Active: &on}, time.Now())
	require.NoError(t, err)
	assert.True(t, updated.Active)
	assert.Zero(t, updated.ConsecutiveFailures)
	assert.Empty(t, updated.DisabledReason)

	// active=false does NOT reset the failure bookkeeping.
	_, _, err = st.RecordFailure(ctx, sub.ID, "http 502", 10, time.Now())
	require.NoError(t, err)
	off := false
	updated, err = st.Update(ctx, "user:1", sub.ID, Patch{Active: &off}, time.Now())
	require.NoError(t, err)
	assert.False(t, updated.Active)
	assert.Equal(t, int64(1), updated.ConsecutiveFailures)
}

func TestStoreRecordSuccessResetsCounter(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sub := mustCreate(t, st, "user:1", NewSubscription{})

	_, disabled, err := st.RecordFailure(ctx, sub.ID, "network: refused", 10, time.Now())
	require.NoError(t, err)
	assert.False(t, disabled)

	require.NoError(t, st.RecordSuccess(ctx, sub.ID, time.Now()))
	got, err := st.Get(ctx, "user:1", sub.ID)
	require.NoError(t, err)
	assert.Zero(t, got.ConsecutiveFailures, "success resets the consecutive counter")
	assert.NotEmpty(t, got.LastSuccessAt)
	assert.True(t, got.Active)
}

func TestStoreDisabledSubscriptionNotEnabled(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sub := mustCreate(t, st, "user:1", NewSubscription{})
	_, disabled, err := st.RecordFailure(ctx, sub.ID, "http 500", 1, time.Now())
	require.NoError(t, err)
	require.True(t, disabled)

	enabled, err := st.ListEnabled(ctx)
	require.NoError(t, err)
	assert.Empty(t, enabled, "auto-disabled subscriptions leave the fan-out set")
}

// TestStoreSurvivesReopen pins the config-DB contract: closing and reopening
// the same file (as across a deploy) keeps every subscription — there is no
// version nuke on this store.
func TestStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscriptions.db")
	st, err := Open(path)
	require.NoError(t, err)
	sub, err := st.Create(context.Background(), "user:1", NewSubscription{
		URL: "https://example.com/hook", Secret: testSecret(),
	}, time.Now())
	require.NoError(t, err)
	require.NoError(t, st.Close())

	st2, err := Open(path)
	require.NoError(t, err)
	defer st2.Close()
	got, err := st2.Get(context.Background(), "user:1", sub.ID)
	require.NoError(t, err)
	assert.Equal(t, sub.URL, got.URL)
	assert.Equal(t, sub.Secret, got.Secret)
}
