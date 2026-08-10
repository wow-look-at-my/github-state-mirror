package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

// TestTimeline_SignInCallsRecorded: a dashboard sign-in makes two real GitHub
// calls -- the OAuth code-for-token exchange and GET /user -- and both are on
// the chart.
//
// They were the mirror's one unobserved outbound path: internal/auth built
// its own http.Client, so the dashboard could not show traffic it was itself
// generating. The observation now hangs off that client's TRANSPORT, which is
// what makes it true of any call the package adds later rather than of these
// two.
func TestTimeline_SignInCallsRecorded(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/user" {
			writeGitHubJSON(w, map[string]any{"login": "someone", "avatar_url": "https://example.test/a.png"})
			return
		}
		writeGitHubJSON(w, map[string]any{"access_token": "gho_signin"})
	}))
	defer github.Close()

	tl := reqtimeline.New()
	svc := auth.New(auth.Config{
		ClientID: "cid", ClientSecret: "secret",
		TokenURL: github.URL + "/login/oauth/access_token", APIBaseURL: github.URL,
		Observer: TimelineLoginObserver(tl),
	})

	token, err := svc.Exchange(context.Background(), "code", "https://example.test/auth/callback")
	require.NoError(t, err)
	login, _, err := svc.FetchLogin(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, "someone", login)

	logins := eventsWhere(tl.Snapshot(0), dispLogin)
	require.Len(t, logins, 2, "both halves of a sign-in are real GitHub calls")
	lanes := []string{logins[0].Lane, logins[1].Lane}
	assert.ElementsMatch(t, []string{"POST /login/oauth/…", "GET /user"}, lanes)
	for _, e := range logins {
		assert.Equal(t, http.StatusOK, e.Status)
		assert.Equal(t, "anonymous", e.Actor, "there is no principal yet: resolving it is what these calls are for")
		assert.GreaterOrEqual(t, e.DurMs, int64(0))
	}
}

// A transport failure on the sign-in path is charted as a failure rather than
// vanishing -- the case where the operator most needs to see the attempt.
func TestTimeline_SignInFailureRecorded(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening now: the exchange dies before a response

	tl := reqtimeline.New()
	svc := auth.New(auth.Config{
		ClientID: "cid", ClientSecret: "secret",
		TokenURL: deadURL + "/login/oauth/access_token", APIBaseURL: deadURL,
		Observer: TimelineLoginObserver(tl),
	})

	_, err := svc.Exchange(context.Background(), "code", "https://example.test/auth/callback")
	require.Error(t, err)

	failed := eventsWhere(tl.Snapshot(0), DispError)
	require.Len(t, failed, 1)
	assert.Equal(t, 0, failed[0].Status, "no response arrived, and that is the point of the record")
}
