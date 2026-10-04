package database

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Writers queued behind a held lock get it in the order they arrived, each
// handed over directly, with no newcomer slipping in between.
func TestWriteGate_AdmitsWritersInArrivalOrder(t *testing.T) {
	g := newWriteGate()
	require.NoError(t, g.acquire(context.Background()))

	const writers = 8
	order := make(chan int, writers)
	for i := range writers {
		go func() {
			if err := g.acquire(context.Background()); err != nil {
				t.Errorf("writer %d: %v", i, err)
				return
			}
			order <- i
			g.release()
		}()
		require.Eventually(t, func() bool { return g.queued() == i+1 }, 5*time.Second, time.Millisecond)
	}

	g.release()
	for want := range writers {
		select {
		case got := <-order:
			assert.Equal(t, want, got, "writer admitted out of arrival order")
		case <-time.After(5 * time.Second):
			t.Fatalf("writer %d never got the gate", want)
		}
	}
	require.NoError(t, g.acquire(context.Background()), "the gate is free once every writer released it")
	g.release()
}

// A waiter whose context ends leaves the queue; the gate still reaches the next writer.
func TestWriteGate_CancelledWaiterLeavesTheQueue(t *testing.T) {
	g := newWriteGate()
	require.NoError(t, g.acquire(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() { gaveUp <- g.acquire(ctx) }()
	require.Eventually(t, func() bool { return g.queued() == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	assert.ErrorIs(t, <-gaveUp, context.Canceled)
	assert.Equal(t, 0, g.queued())

	g.release()
	require.NoError(t, g.acquire(context.Background()))
	g.release()
}

// The whole point of the gate's holder tracking: a request sent from inside a
// transaction is refused and recorded, while the same request from outside
// one, or from another goroutine during it, goes through.
func TestNoWriteHeldTransport_RefusesARequestSentInsideATransaction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client := &http.Client{Transport: NoWriteHeldTransport(http.DefaultTransport)}
	get := func() error {
		resp, err := client.Get(srv.URL)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	before := len(WriteHeldRequests())
	require.NoError(t, get(), "no transaction open: the request goes through")

	tx, err := db.Begin()
	require.NoError(t, err)
	otherGoroutine := make(chan error, 1)
	go func() { otherGoroutine <- get() }()
	assert.NoError(t, <-otherGoroutine, "another goroutine does not hold this transaction")

	err = get()
	assert.True(t, errors.Is(err, ErrRequestUnderWriteLock), "a request inside the transaction must be refused, got %v", err)
	require.NoError(t, tx.Commit())
	assert.Len(t, WriteHeldRequests(), before+1, "the refused request is recorded for TestMain to fail on")

	require.NoError(t, get(), "committed: the request goes through again")

	// An autocommit write that returns rows holds the lock until its rows close.
	rows, err := db.Query("INSERT INTO schema_version (fingerprint) VALUES ('x') RETURNING fingerprint")
	require.NoError(t, err)
	assert.True(t, errors.Is(get(), ErrRequestUnderWriteLock), "the RETURNING write still holds the lock")
	require.NoError(t, rows.Close())
	require.NoError(t, get())
}

func TestIsWriteSQL(t *testing.T) {
	for q, want := range map[string]bool{
		"-- name: ClaimWebhookWatermark :one\nINSERT INTO t VALUES (1) RETURNING a": true,
		"/* c */ update t set a = 1 returning a":                                    true,
		"DELETE FROM t RETURNING a":                                                  true,
		"WITH x AS (SELECT 1) DELETE FROM t WHERE a IN x RETURNING a":                true,
		"-- name: GetRepo :one\nSELECT * FROM repos WHERE owner = ?":                 false,
		"WITH x AS (SELECT 1) SELECT * FROM x":                                       false,
		"PRAGMA busy_timeout":                                                        false,
	} {
		assert.Equal(t, want, isWriteSQL(q), q)
	}
}
