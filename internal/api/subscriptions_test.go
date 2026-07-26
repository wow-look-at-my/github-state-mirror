package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/notify"
)

// twoUserGH is a fake GitHub that resolves the standard test token and
// "other-user-token" to two distinct users.
func twoUserGH() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer " + testToken:
			_ = json.NewEncoder(w).Encode(map[string]any{"login": testUserLogin, "id": testUserID})
		case "Bearer other-user-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "otheruser", "id": 8002})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
}

const testSubSecret = "0123456789abcdef0123456789abcdef"

// subReq performs an authenticated /_mirror/subscriptions request and returns
// the recorder.
func subReq(router http.Handler, token, method, target, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestSubscriptionsCRUD(t *testing.T) {
	stack := newFullTestStack(t, auth.New(auth.Config{SessionKey: []byte("test-session-key")}), twoUserGH())

	// Unauthenticated requests are rejected by requireAuth.
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(m, "/_mirror/subscriptions", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		stack.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s without a token must be 401", m)
	}

	// Create.
	w := subReq(stack.router, testToken, http.MethodPost, "/_mirror/subscriptions",
		fmt.Sprintf(`{"url":"https://example.com/hook","secret":%q,"repos":["My-Org"],"events":["push","pull_request"]}`, testSubSecret))
	require.Equal(t, http.StatusCreated, w.Code, "create: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), testSubSecret, "the secret must never appear in a response")
	var created subscriptionJSON
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.True(t, strings.HasPrefix(created.ID, "sub_"))
	assert.Equal(t, []string{"my-org"}, created.Repos, "repo filters come back lowercased")
	assert.Equal(t, []string{"push", "pull_request"}, created.Events)
	assert.True(t, created.Active)
	assert.Empty(t, created.Principal, "the caller view carries no principal field")

	// A response decoded as a raw map must not have a secret key either.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "secret")

	// List (own only).
	w = subReq(stack.router, testToken, http.MethodGet, "/_mirror/subscriptions", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), testSubSecret)
	var listed struct {
		Subscriptions []subscriptionJSON `json:"subscriptions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listed))
	require.Len(t, listed.Subscriptions, 1)
	assert.Equal(t, created.ID, listed.Subscriptions[0].ID)

	// Get.
	w = subReq(stack.router, testToken, http.MethodGet, "/_mirror/subscriptions/"+created.ID, "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), testSubSecret)

	// A second principal gets 404 for the first's id — and an empty list.
	w = subReq(stack.router, "other-user-token", http.MethodGet, "/_mirror/subscriptions/"+created.ID, "")
	assert.Equal(t, http.StatusNotFound, w.Code, "a foreign principal's id answers 404")
	w = subReq(stack.router, "other-user-token", http.MethodDelete, "/_mirror/subscriptions/"+created.ID, "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	w = subReq(stack.router, "other-user-token", http.MethodPatch, "/_mirror/subscriptions/"+created.ID, `{"active":false}`)
	assert.Equal(t, http.StatusNotFound, w.Code)
	w = subReq(stack.router, "other-user-token", http.MethodGet, "/_mirror/subscriptions", "")
	require.Equal(t, http.StatusOK, w.Code)
	var otherList struct {
		Subscriptions []subscriptionJSON `json:"subscriptions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &otherList))
	assert.Empty(t, otherList.Subscriptions)

	// Patch: change the URL, deactivate.
	w = subReq(stack.router, testToken, http.MethodPatch, "/_mirror/subscriptions/"+created.ID,
		`{"url":"https://example.com/hook2","active":false}`)
	require.Equal(t, http.StatusOK, w.Code, "patch: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), testSubSecret)
	var patched subscriptionJSON
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &patched))
	assert.Equal(t, "https://example.com/hook2", patched.URL)
	assert.False(t, patched.Active)
	assert.Equal(t, created.Repos, patched.Repos, "unpatched fields survive")

	// Validation failures answer 400 with the reason.
	w = subReq(stack.router, testToken, http.MethodPatch, "/_mirror/subscriptions/"+created.ID,
		`{"url":"http://not-loopback.example.com/x"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "loopback")
	w = subReq(stack.router, testToken, http.MethodPost, "/_mirror/subscriptions",
		`{"url":"https://example.com/x","secret":"short"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "secret")
	w = subReq(stack.router, testToken, http.MethodPost, "/_mirror/subscriptions", `{not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Delete.
	w = subReq(stack.router, testToken, http.MethodDelete, "/_mirror/subscriptions/"+created.ID, "")
	assert.Equal(t, http.StatusNoContent, w.Code)
	w = subReq(stack.router, testToken, http.MethodGet, "/_mirror/subscriptions/"+created.ID, "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSubscriptionsPerPrincipalCap(t *testing.T) {
	stack := newFullTestStack(t, auth.New(auth.Config{SessionKey: []byte("test-session-key")}), twoUserGH())

	// Fill the cap directly through the store (same principal requireAuth
	// resolves the test token to).
	st := stack.notifier.Store()
	for i := 0; i < notify.MaxPerPrincipal; i++ {
		_, err := st.Create(context.Background(), testUserActor, notify.NewSubscription{
			URL: "https://example.com/hook", Secret: testSubSecret,
		}, time.Now())
		require.NoError(t, err)
	}

	w := subReq(stack.router, testToken, http.MethodPost, "/_mirror/subscriptions",
		fmt.Sprintf(`{"url":"https://example.com/hook","secret":%q}`, testSubSecret))
	assert.Equal(t, http.StatusConflict, w.Code, "the per-principal cap answers 409")
}

// TestWebhookDeliveryNotifiesSubscriber drives the WHOLE wiring: a signed
// GitHub webhook POSTed to the real router is dispatched to global truth,
// then the subscriber registered over the CRUD API receives a signed
// notification.
func TestWebhookDeliveryNotifiesSubscriber(t *testing.T) {
	stack := newFullTestStack(t, auth.New(auth.Config{SessionKey: []byte("test-session-key")}), twoUserGH())

	// Truth knows the repo is public, so any principal's subscription is
	// revealed the delivery.
	require.NoError(t, stack.store.UpsertRepo(context.Background(), dbgen.Repo{
		Owner: "my-org", Name: "open", NameWithOwner: "my-org/open", Url: "u",
		Visibility: ghdata.VisibilityPublic,
	}))

	var mu sync.Mutex
	var gotBody []byte
	var gotHeader http.Header
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody, gotHeader = b, r.Header.Clone()
		mu.Unlock()
	}))
	defer receiver.Close()

	w := subReq(stack.router, testToken, http.MethodPost, "/_mirror/subscriptions",
		fmt.Sprintf(`{"url":%q,"secret":%q,"events":["push"]}`, receiver.URL, testSubSecret))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	// A signed push delivery through the real /webhook route.
	payload := `{"ref":"refs/heads/master","before":"` + strings.Repeat("1", 40) + `","after":"` + strings.Repeat("2", 40) + `","repository":{"name":"open","owner":{"login":"my-org"}}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "gh-guid-e2e")
	req.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, []byte(payload)))
	rw := httptest.NewRecorder()
	stack.router.ServeHTTP(rw, req)
	require.Equal(t, http.StatusOK, rw.Code, "the push applies to global truth")

	require.True(t, stack.notifier.Flush(5*time.Second), "notification must complete")

	mu.Lock()
	body, hdr := gotBody, gotHeader
	mu.Unlock()
	require.NotNil(t, body, "the subscriber endpoint was notified")

	mac := hmac.New(sha256.New, []byte(testSubSecret))
	mac.Write(body)
	assert.Equal(t, "sha256="+hex.EncodeToString(mac.Sum(nil)), hdr.Get("X-Hub-Signature-256"))

	var note notify.Notification
	require.NoError(t, json.Unmarshal(body, &note))
	assert.Equal(t, "push", note.Event)
	assert.Equal(t, "my-org/open", note.RepoFullName)
	assert.Equal(t, "gh-guid-e2e", note.GitHubDelivery)
	assert.Equal(t, "refs/heads/master", note.Ref)
	assert.Equal(t, strings.Repeat("2", 40), note.SHA)
	assert.Equal(t, "applied", note.Disposition)
}

func TestNotificationsAdminEndpoint(t *testing.T) {
	svc := configuredAuth(t)
	stack := newFullTestStack(t, svc, twoUserGH())

	// A live receiver so a delivery attempt lands in the activity ring.
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer receiver.Close()

	_, err := stack.notifier.Store().Create(context.Background(), "user:42", notify.NewSubscription{
		URL: receiver.URL, Secret: testSubSecret,
	}, time.Now())
	require.NoError(t, err)

	// A recorded identity for the subscription's principal: the operator view
	// resolves it to the login.
	require.NoError(t, stack.store.RecordActorIdentity(context.Background(), "user:42", "octocat"))

	// Drive one delivery so the recent-attempts ring has an entry to decorate.
	require.NoError(t, stack.store.UpsertRepo(context.Background(), dbgen.Repo{
		Owner: "my-org", Name: "open", NameWithOwner: "my-org/open", Url: "u",
		Visibility: ghdata.VisibilityPublic,
	}))
	payload := `{"ref":"refs/heads/master","before":"` + strings.Repeat("1", 40) + `","after":"` + strings.Repeat("2", 40) + `","repository":{"name":"open","owner":{"login":"my-org"}}}`
	wh := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	wh.Header.Set("X-GitHub-Event", "push")
	wh.Header.Set("X-GitHub-Delivery", "gh-guid-admin")
	wh.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, []byte(payload)))
	whw := httptest.NewRecorder()
	stack.router.ServeHTTP(whw, wh)
	require.Equal(t, http.StatusOK, whw.Code)
	require.True(t, stack.notifier.Flush(5*time.Second), "delivery must complete")

	// Anonymous: 401. Signed-in non-admin: 403.
	req := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	w := httptest.NewRecorder()
	stack.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	req = httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	req.AddCookie(mintSession(t, svc, "somebody"))
	w = httptest.NewRecorder()
	stack.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)

	// Admin: the operator view — counters, recent, ALL subscriptions with
	// principals, secrets structurally absent.
	req = httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w = httptest.NewRecorder()
	stack.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), testSubSecret, "the admin view never carries secrets")

	var resp struct {
		Counters      notify.Counters    `json:"counters"`
		Recent        []notify.Attempt   `json:"recent"`
		Subscriptions []subscriptionJSON `json:"subscriptions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Subscriptions, 1)
	assert.Equal(t, "user:42", resp.Subscriptions[0].Principal, "the operator view names the principal")
	assert.Equal(t, "octocat", resp.Subscriptions[0].PrincipalName, "the recorded login decorates the principal")
	require.NotEmpty(t, resp.Recent, "the driven delivery must appear in the activity ring")
	assert.Equal(t, "user:42", resp.Recent[0].Principal)
	assert.Equal(t, "octocat", resp.Recent[0].PrincipalName, "attempts are decorated with the recorded login")
}
