package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// fakeAccess is an in-memory AccessChecker: visibility per lowercased
// "owner/repo", grants per "principal|owner/repo".
type fakeAccess struct {
	mu         sync.Mutex
	visibility map[string]string
	grants     map[string]bool
	grantErr   error
}

func newFakeAccess() *fakeAccess {
	return &fakeAccess{visibility: map[string]string{}, grants: map[string]bool{}}
}

func (f *fakeAccess) setVisibility(owner, repo, vis string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visibility[strings.ToLower(owner+"/"+repo)] = vis
}

func (f *fakeAccess) grant(principal, owner, repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants[principal+"|"+strings.ToLower(owner+"/"+repo)] = true
}

func (f *fakeAccess) GetRepoInsensitive(_ context.Context, owner, name string) (dbgen.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vis, ok := f.visibility[strings.ToLower(owner+"/"+name)]
	if !ok {
		return dbgen.Repo{}, sql.ErrNoRows
	}
	return dbgen.Repo{Owner: owner, Name: name, Visibility: vis}, nil
}

func (f *fakeAccess) HasGrant(_ context.Context, principal, owner, repo string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grantErr != nil {
		return false, f.grantErr
	}
	return f.grants[principal+"|"+strings.ToLower(owner+"/"+repo)], nil
}

// newTestNotifier builds a notifier over a temp store with fast test tunables
// (override via mutate).
func newTestNotifier(t *testing.T, access AccessChecker, mutate func(*Config)) (*Notifier, *Store) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "subscriptions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	cfg := Config{
		Store:          st,
		Access:         access,
		Attempts:       1,
		AttemptTimeout: 5 * time.Second,
		Backoff:        []time.Duration{time.Millisecond},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	n := New(cfg)
	t.Cleanup(func() { n.Drain(5 * time.Second) })
	return n, st
}

// prEvent builds a parsed pull_request event the way the webhook handler does.
func prEvent(owner, repo string, num int, headSHA string) webhook.Event {
	raw := mustJSON(map[string]any{
		"action": "opened",
		"pull_request": map[string]any{
			"number": num,
			"head":   map[string]any{"sha": headSHA, "ref": "feature"},
		},
		"repository": map[string]any{"name": repo, "owner": map[string]any{"login": owner}},
	})
	e := webhook.ParseEvent("pull_request", raw)
	e.DeliveryID = "mirror-received-guid"
	return e
}

func pushEvent(owner, repo, ref, after string) webhook.Event {
	raw := mustJSON(map[string]any{
		"ref": ref, "after": after,
		"repository": map[string]any{"name": repo, "owner": map[string]any{"login": owner}},
	})
	e := webhook.ParseEvent("push", raw)
	e.DeliveryID = "mirror-received-guid-push"
	return e
}

func workflowRunEvent(owner, repo, headSHA string) webhook.Event {
	raw := mustJSON(map[string]any{
		"action":       "completed",
		"workflow_run": map[string]any{"id": 123, "head_sha": headSHA},
		"repository":   map[string]any{"name": repo, "owner": map[string]any{"login": owner}},
	})
	e := webhook.ParseEvent("workflow_run", raw)
	e.DeliveryID = "mirror-received-guid-workflow-run"
	return e
}

func applied() webhook.DispatchResult {
	return webhook.DispatchResult{Disposition: webhook.DispApplied}
}

// capture is a receiver that records every request body + headers.
type capture struct {
	mu       sync.Mutex
	bodies   [][]byte
	headers  []http.Header
	respond  int
	requests atomic.Int64
}

func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.headers = append(c.headers, r.Header.Clone())
		c.mu.Unlock()
		c.requests.Add(1)
		status := c.respond
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}
}

func (c *capture) first(t *testing.T) ([]byte, http.Header) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotEmpty(t, c.bodies, "receiver saw no request")
	return c.bodies[0], c.headers[0]
}

func TestNotifierSignatureAndPayloadShape(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "repo1", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, nil)

	rec := &capture{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	secret := strings.Repeat("k", 32)
	sub, err := st.Create(context.Background(), "user:1", NewSubscription{URL: srv.URL, Secret: secret}, time.Now())
	require.NoError(t, err)

	ingestedAt := time.Now().Add(-2 * time.Second)
	n.NotifyIngest(prEvent("My-Org", "Repo1", 42, "abc123def456"), applied(), ingestedAt)
	require.True(t, n.Flush(5*time.Second), "delivery must finish")

	body, hdr := rec.first(t)

	// Headers: GitHub's exact signature scheme over the exact raw body.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	assert.Equal(t, "sha256="+hex.EncodeToString(mac.Sum(nil)), hdr.Get("X-Hub-Signature-256"),
		"X-Hub-Signature-256 must be HMAC-SHA256 over the exact raw body")
	assert.Equal(t, "application/json", hdr.Get("Content-Type"))
	assert.Equal(t, "pull_request", hdr.Get("X-Mirror-Event"))
	assert.Contains(t, hdr.Get("User-Agent"), "github-state-mirror")

	// Payload: every field.
	var note Notification
	require.NoError(t, json.Unmarshal(body, &note))
	assert.True(t, strings.HasPrefix(note.MirrorDelivery, "ntf_"))
	assert.Len(t, note.MirrorDelivery, len("ntf_")+32)
	assert.Equal(t, note.MirrorDelivery, hdr.Get("X-Mirror-Delivery"))
	assert.Equal(t, sub.ID, note.SubscriptionID)
	assert.Equal(t, "mirror-received-guid", note.GitHubDelivery)
	assert.Equal(t, "pull_request", note.Event)
	assert.Equal(t, "opened", note.Action)
	assert.Equal(t, "My-Org", note.Owner)
	assert.Equal(t, "Repo1", note.Repo)
	assert.Equal(t, "My-Org/Repo1", note.RepoFullName)
	assert.Equal(t, int64(42), note.PRNumber)
	assert.Equal(t, "abc123def456", note.SHA, "a PR event carries the head sha")
	assert.Equal(t, webhook.DispApplied, note.Disposition)
	assert.Equal(t, ingestedAt.UTC().Format(time.RFC3339Nano), note.IngestedAt)
	sentAt, err := time.Parse(time.RFC3339Nano, note.SentAt)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), sentAt, time.Minute)

	// Absent identifiers are omitted, not zero-valued.
	var asMap map[string]any
	require.NoError(t, json.Unmarshal(body, &asMap))
	assert.NotContains(t, asMap, "ref", "a PR event has no ref field")

	// Success bookkeeping.
	got, err := st.Get(context.Background(), "user:1", sub.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, got.LastSuccessAt)
	counters, recent := n.Activity(10)
	assert.Equal(t, int64(1), counters.Delivered)
	require.Len(t, recent, 1)
	assert.Equal(t, OutcomeDelivered, recent[0].Outcome)
	assert.Equal(t, http.StatusOK, recent[0].HTTPStatus)
}

func TestNotifierPushPayloadIdentifiers(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "repo1", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, nil)

	rec := &capture{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	_, err := st.Create(context.Background(), "user:1", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	n.NotifyIngest(pushEvent("my-org", "repo1", "refs/heads/master", "feedbeef"), applied(), time.Now())
	require.True(t, n.Flush(5*time.Second))

	body, _ := rec.first(t)
	var note Notification
	require.NoError(t, json.Unmarshal(body, &note))
	assert.Equal(t, "push", note.Event)
	assert.Equal(t, "refs/heads/master", note.Ref)
	assert.Equal(t, "feedbeef", note.SHA, "a push carries the after sha")
	var asMap map[string]any
	require.NoError(t, json.Unmarshal(body, &asMap))
	assert.NotContains(t, asMap, "pr_number")
}

// TestNotifierWorkflowRunPayloadIdentifiers: a workflow_run notification
// carries the run's head sha (via webhook.ParseWorkflowRunHeadSHA), the same
// correlation field a status/check_run notification carries — a required-
// builds-manager-style consumer keys off owner/repo/sha regardless of which
// of the three build listing types the event is.
func TestNotifierWorkflowRunPayloadIdentifiers(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "repo1", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, nil)

	rec := &capture{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	_, err := st.Create(context.Background(), "user:1", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	n.NotifyIngest(workflowRunEvent("my-org", "repo1", "feedbeef"), applied(), time.Now())
	require.True(t, n.Flush(5*time.Second))

	body, _ := rec.first(t)
	var note Notification
	require.NoError(t, json.Unmarshal(body, &note))
	assert.Equal(t, "workflow_run", note.Event)
	assert.Equal(t, "feedbeef", note.SHA, "a workflow_run carries the run's head sha")
}

// TestNotifierRevealGating is the load-bearing security test: a private
// repo's activity must never reach a principal without a live grant.
func TestNotifierRevealGating(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "secret", ghdata.VisibilityPrivate)
	access.grant("user:granted", "my-org", "secret")
	n, st := newTestNotifier(t, access, nil)

	grantedRec, ungrantedRec := &capture{}, &capture{}
	grantedSrv := httptest.NewServer(grantedRec.handler())
	defer grantedSrv.Close()
	ungrantedSrv := httptest.NewServer(ungrantedRec.handler())
	defer ungrantedSrv.Close()

	ctx := context.Background()
	_, err := st.Create(ctx, "user:granted", NewSubscription{URL: grantedSrv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)
	_, err = st.Create(ctx, "user:ungranted", NewSubscription{URL: ungrantedSrv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	n.NotifyIngest(prEvent("my-org", "secret", 7, "aaa"), applied(), time.Now())
	require.True(t, n.Flush(5*time.Second), "full quiescence before asserting")

	assert.EqualValues(t, 1, grantedRec.requests.Load(), "the granted principal's receiver gets the delivery")
	assert.Zero(t, ungrantedRec.requests.Load(), "the ungranted principal's receiver must see ZERO requests")

	counters, _ := n.Activity(10)
	assert.Equal(t, int64(1), counters.Delivered)
	assert.Equal(t, int64(1), counters.Gated)
}

func TestNotifierPublicRepoDeliveredToAnyone(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "open", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, nil)

	rec := &capture{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	// No grants at all: public visibility alone reveals.
	_, err := st.Create(context.Background(), "user:anyone", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	n.NotifyIngest(prEvent("my-org", "open", 1, "bbb"), applied(), time.Now())
	require.True(t, n.Flush(5*time.Second))
	assert.EqualValues(t, 1, rec.requests.Load())
}

func TestNotifierUnknownVisibilityGated(t *testing.T) {
	// The repo is absent from truth entirely: fail closed, grant-only.
	access := newFakeAccess()
	n, st := newTestNotifier(t, access, nil)

	rec := &capture{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	_, err := st.Create(context.Background(), "user:1", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	n.NotifyIngest(prEvent("my-org", "mystery", 1, "ccc"), applied(), time.Now())
	require.True(t, n.Flush(5*time.Second))
	assert.Zero(t, rec.requests.Load(), "unknown visibility is private: gated without a grant")

	// A grant-lookup ERROR also fails closed.
	access.setVisibility("my-org", "mystery", ghdata.VisibilityPrivate)
	access.grant("user:1", "my-org", "mystery")
	access.mu.Lock()
	access.grantErr = fmt.Errorf("db broken")
	access.mu.Unlock()
	n.NotifyIngest(prEvent("my-org", "mystery", 2, "ddd"), applied(), time.Now())
	require.True(t, n.Flush(5*time.Second))
	assert.Zero(t, rec.requests.Load(), "a reveal-store failure gates the notification")

	counters, _ := n.Activity(10)
	assert.Equal(t, int64(2), counters.Gated)
}

func TestNotifierDispositionAndRepoFiltering(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "repo1", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, nil)

	rec := &capture{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	_, err := st.Create(context.Background(), "user:1", NewSubscription{URL: srv.URL, Secret: testSecret()}, time.Now())
	require.NoError(t, err)

	ev := prEvent("my-org", "repo1", 1, "e1")
	// ignored and error dispositions never notify.
	n.NotifyIngest(ev, webhook.DispatchResult{Disposition: webhook.DispIgnored}, time.Now())
	n.NotifyIngest(ev, webhook.DispatchResult{Disposition: webhook.DispError}, time.Now())
	// A repo-less delivery (installation and friends) never notifies.
	repoless := webhook.ParseEvent("installation", []byte(`{"action":"created","installation":{"id":1}}`))
	n.NotifyIngest(repoless, applied(), time.Now())
	require.True(t, n.Flush(5*time.Second))
	assert.Zero(t, rec.requests.Load())

	// invalidated DOES notify (it changed what callers may see).
	n.NotifyIngest(ev, webhook.DispatchResult{Disposition: webhook.DispInvalidated}, time.Now())
	require.True(t, n.Flush(5*time.Second))
	assert.EqualValues(t, 1, rec.requests.Load())
	body, _ := rec.first(t)
	var note Notification
	require.NoError(t, json.Unmarshal(body, &note))
	assert.Equal(t, webhook.DispInvalidated, note.Disposition)
}

func TestNotifierEventAndRepoFilterFanOut(t *testing.T) {
	access := newFakeAccess()
	access.setVisibility("my-org", "a", ghdata.VisibilityPublic)
	access.setVisibility("other-org", "b", ghdata.VisibilityPublic)
	n, st := newTestNotifier(t, access, nil)

	rec := &capture{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	_, err := st.Create(context.Background(), "user:1", NewSubscription{
		URL: srv.URL, Secret: testSecret(),
		Repos:  []string{"my-org"},
		Events: []string{"push"},
	}, time.Now())
	require.NoError(t, err)

	// Wrong event type: filtered.
	n.NotifyIngest(prEvent("my-org", "a", 1, "s1"), applied(), time.Now())
	// Wrong owner: filtered.
	n.NotifyIngest(pushEvent("other-org", "b", "refs/heads/x", "s2"), applied(), time.Now())
	require.True(t, n.Flush(5*time.Second))
	assert.Zero(t, rec.requests.Load())

	// Matching event + owner filter (case-insensitive): delivered.
	n.NotifyIngest(pushEvent("My-Org", "a", "refs/heads/x", "s3"), applied(), time.Now())
	require.True(t, n.Flush(5*time.Second))
	assert.EqualValues(t, 1, rec.requests.Load())
}

// TestNotifierNonBlocking proves NotifyIngest returns while the subscriber
// endpoint is still holding the connection open.

// mustJSON marshals a delivery body. JSON text is produced by marshalling,
// never assembled from string pieces (internal/guards).
func mustJSON(payload map[string]any) []byte {
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return body
}
