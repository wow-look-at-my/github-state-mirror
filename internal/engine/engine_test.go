package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

func TestDeriveDBPath(t *testing.T) {
	assert.Equal(t, "github-mirror-engine.db", DeriveDBPath("github-mirror.db"))
	assert.Equal(t, "/data/cache-engine.db", DeriveDBPath("/data/cache"))
}

func TestNew_LoadsTheEmbeddedSpec(t *testing.T) {
	g, err := New(context.Background(), Options{DB: filepath.Join(t.TempDir(), "e.db"), Base: "https://api.example.com/"}, "s")
	require.NoError(t, err)
	assert.True(t, g.Declares(http.MethodGet, "/repos/o/r/tags"))
	assert.True(t, g.Declares(http.MethodGet, "/repos/o/r/rules/branches/release/v1"))
	assert.False(t, g.Declares(http.MethodGet, "/repos/o/r/branches"))
	g.Start()
	assert.True(t, g.Drain(5*time.Second))
	assert.NoError(t, g.Close())
}

func TestNew_RequiresDBAndBase(t *testing.T) {
	_, err := New(context.Background(), Options{Base: "https://api.example.com"}, "s")
	assert.Error(t, err)
	_, err = New(context.Background(), Options{DB: filepath.Join(t.TempDir(), "e.db")}, "s")
	assert.Error(t, err)
}

// fakeEvents records each forwarded delivery and answers with status.
type fakeEvents struct {
	mu     sync.Mutex
	got    []*http.Request
	bodies [][]byte
	status int
	answer string
}

func (f *fakeEvents) EventsPath() string { return "/events" }

func (f *fakeEvents) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.got = append(f.got, r)
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	w.WriteHeader(f.status)
	if f.answer != "" {
		_, _ = io.WriteString(w, f.answer+"\n")
	}
}

func TestApplier_AnIgnoredDeliveryReportsTheEnginesOutcome(t *testing.T) {
	for _, tc := range []struct{ own, engine, want string }{
		{webhook.DispIgnored, webhook.DispInvalidated, webhook.DispInvalidated},
		{webhook.DispIgnored, webhook.DispApplied, webhook.DispApplied},
		{webhook.DispIgnored, webhook.DispIgnored, webhook.DispIgnored},
		{webhook.DispApplied, webhook.DispInvalidated, webhook.DispApplied},
	} {
		ev := &fakeEvents{status: http.StatusAccepted, answer: tc.engine}
		res := newApplier(&ownApply{disposition: tc.own}, ev, "secret").
			Dispatch(context.Background(), webhook.Event{Type: "repository_ruleset", DeliveryID: "d"})
		assert.Equal(t, tc.want, res.Disposition, "own %s, engine %s", tc.own, tc.engine)
	}
}

func TestApplier_AmendsTheLogOnlyWhenTheEngineChangedTheOutcome(t *testing.T) {
	type amended struct{ id, disposition, detail string }
	for _, tc := range []struct {
		own, engine string
		status      int
		want        *amended
	}{
		{webhook.DispIgnored, webhook.DispInvalidated, http.StatusAccepted, &amended{"d", webhook.DispInvalidated, "engine: invalidated"}},
		{webhook.DispApplied, "", http.StatusForbidden, &amended{"d", webhook.DispError, "engine: status 403: "}},
		{webhook.DispApplied, webhook.DispInvalidated, http.StatusAccepted, nil},
		{webhook.DispIgnored, webhook.DispIgnored, http.StatusAccepted, nil},
	} {
		var got *amended
		a := newApplier(&ownApply{disposition: tc.own}, &fakeEvents{status: tc.status, answer: tc.engine}, "secret")
		a.amend = func(_ context.Context, id, disposition, detail string) { got = &amended{id, disposition, detail} }
		a.Dispatch(context.Background(), webhook.Event{Type: "repository_ruleset", DeliveryID: "d"})
		assert.Equal(t, tc.want, got, "own %s, engine %s %d", tc.own, tc.engine, tc.status)
	}
}

// ownApply stands in for the service's dispatcher and answers with one disposition.
type ownApply struct {
	disposition string
	calls       int
}

func (o *ownApply) Dispatch(context.Context, webhook.Event) webhook.DispatchResult {
	o.calls++
	return webhook.DispatchResult{Disposition: o.disposition}
}

func TestApplier_TheEngineHasEachDeliveryWhenDispatchReturns(t *testing.T) {
	ev := &fakeEvents{status: http.StatusAccepted}
	own := &ownApply{disposition: webhook.DispApplied}
	a := newApplier(own, ev, "secret")
	for n, id := range []string{"d1", "d2", "d3"} {
		raw, err := json.Marshal(map[string]string{"id": id})
		require.NoError(t, err)
		res := a.Dispatch(context.Background(), webhook.Event{Type: "create", DeliveryID: id, Raw: raw})
		assert.Equal(t, webhook.DispApplied, res.Disposition, "the service's own outcome stands when the engine accepts")
		require.Len(t, ev.got, n+1, "the engine has the delivery before the worker marks it applied")
	}
	assert.Equal(t, 3, own.calls)
	for i, id := range []string{"d1", "d2", "d3"} {
		r := ev.got[i]
		assert.Equal(t, "/events", r.URL.Path)
		assert.Equal(t, id, r.Header.Get("X-GitHub-Delivery"))
		assert.Equal(t, "create", r.Header.Get("X-GitHub-Event"))
		mac := hmac.New(sha256.New, []byte("secret"))
		mac.Write(ev.bodies[i])
		assert.Equal(t, "sha256="+hex.EncodeToString(mac.Sum(nil)), r.Header.Get("X-Hub-Signature-256"))
	}
}

func TestApplier_AnEngineRefusalIsAnErrorResult(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ev := &fakeEvents{status: http.StatusForbidden}
	a := newApplier(&ownApply{disposition: webhook.DispApplied}, ev, "secret")
	res := a.Dispatch(context.Background(), webhook.Event{Type: "delete", DeliveryID: "d-refused"})
	assert.Equal(t, webhook.DispError, res.Disposition, "the worker tries a delivery the engine refused again")
	assert.Contains(t, res.Detail, "engine: status 403")
	out := buf.String()
	assert.Contains(t, out, "level=ERROR")
	assert.Contains(t, out, "delivery=d-refused")
	assert.Contains(t, out, "type=delete")

	own := newApplier(&ownApply{disposition: webhook.DispError}, &fakeEvents{status: http.StatusAccepted}, "secret").
		Dispatch(context.Background(), webhook.Event{Type: "push", DeliveryID: "d-own"})
	assert.Equal(t, webhook.DispError, own.Disposition, "the service's own error stands when the engine accepts")
}
