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
	block  chan struct{}
}

func (f *fakeEvents) EventsPath() string { return "/events" }

func (f *fakeEvents) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.block != nil {
		<-f.block
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.got = append(f.got, r)
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	w.WriteHeader(f.status)
}

func TestForwarder_SignsAndKeepsOrder(t *testing.T) {
	ev := &fakeEvents{status: http.StatusAccepted}
	f := newForwarder(ev, "secret")
	f.start()
	for _, id := range []string{"d1", "d2", "d3"} {
		raw, err := json.Marshal(map[string]string{"id": id})
		require.NoError(t, err)
		f.NotifyIngest(webhook.Event{Type: "create", DeliveryID: id, Raw: raw}, webhook.DispatchResult{}, time.Now())
	}
	require.True(t, f.Idle(5*time.Second))
	require.Len(t, ev.got, 3)
	for i, id := range []string{"d1", "d2", "d3"} {
		r := ev.got[i]
		assert.Equal(t, "/events", r.URL.Path)
		assert.Equal(t, id, r.Header.Get("X-GitHub-Delivery"))
		assert.Equal(t, "create", r.Header.Get("X-GitHub-Event"))
		mac := hmac.New(sha256.New, []byte("secret"))
		mac.Write(ev.bodies[i])
		assert.Equal(t, "sha256="+hex.EncodeToString(mac.Sum(nil)), r.Header.Get("X-Hub-Signature-256"))
	}
	assert.True(t, f.drain(5*time.Second))
}

func TestForwarder_NeverBlocksTheWorker(t *testing.T) {
	ev := &fakeEvents{status: http.StatusAccepted, block: make(chan struct{})}
	f := newForwarder(ev, "secret")
	f.start()
	done := make(chan struct{})
	go func() {
		for range 3 {
			f.NotifyIngest(webhook.Event{Type: "push", DeliveryID: "d"}, webhook.DispatchResult{}, time.Now())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "NotifyIngest waited on the engine")
	}
	close(ev.block)
	assert.True(t, f.drain(5*time.Second))
}

func TestForwarder_LogsARefusalAtErrorLevel(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ev := &fakeEvents{status: http.StatusForbidden}
	f := newForwarder(ev, "secret")
	f.start()
	f.NotifyIngest(webhook.Event{Type: "delete", DeliveryID: "d-refused"}, webhook.DispatchResult{}, time.Now())
	require.True(t, f.drain(5*time.Second))
	out := buf.String()
	assert.Contains(t, out, "level=ERROR")
	assert.Contains(t, out, "delivery=d-refused")
	assert.Contains(t, out, "type=delete")

	buf.Reset()
	f.NotifyIngest(webhook.Event{Type: "push", DeliveryID: "d-late"}, webhook.DispatchResult{}, time.Now())
	assert.Contains(t, buf.String(), "delivery=d-late", "a delivery after drain is reported, not dropped quietly")
}
