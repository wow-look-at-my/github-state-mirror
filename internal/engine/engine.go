// Package engine runs the embedded api-mirror engine that serves the routes github.xml declares.

package engine

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	apimirror "github.com/wow-look-at-my/api-mirror/engine"
)

//go:embed github.xml
var specSource []byte

// basePlaceholder is the upstream base in github.xml. New replaces it, so the engine asks the same upstream as the rest of this service.
const basePlaceholder = "GSM_UPSTREAM_BASE"

// DeriveDBPath names the engine's database next to the cache DB: a trailing .db is stripped, then -engine.db is appended.
func DeriveDBPath(dbPath string) string {
	return strings.TrimSuffix(dbPath, ".db") + "-engine.db"
}

// Options configures New.
type Options struct {
	// DB is the engine's own SQLite file. The engine rebuilds it when its schema changes.
	DB string
	// Base is the GitHub API base URL.
	Base string
	// OnExchange receives every outbound request the engine sends.
	OnExchange func(apimirror.Exchange)
}

// Engine is the embedded engine plus the delivery forwarder.
type Engine struct {
	e    *apimirror.Engine
	sink atomic.Pointer[func(apimirror.Request)]
	fwd  *Forwarder
}

// New builds the engine from the embedded spec. secret signs the deliveries the forwarder posts.
func New(ctx context.Context, opts Options, secret string) (*Engine, error) {
	if opts.DB == "" || opts.Base == "" {
		return nil, fmt.Errorf("engine: DB and Base are required")
	}
	if n := bytes.Count(specSource, []byte(basePlaceholder)); n != 1 {
		return nil, fmt.Errorf("engine: github.xml must name %s exactly once, found %d", basePlaceholder, n)
	}
	spec := bytes.Replace(specSource, []byte(basePlaceholder), []byte(strings.TrimRight(opts.Base, "/")), 1)
	g := &Engine{}
	inner, err := apimirror.New(ctx, "github.xml", spec, apimirror.Options{
		DB:         opts.DB,
		OnExchange: opts.OnExchange,
		OnRequest:  g.request,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	g.e = inner
	g.fwd = newForwarder(inner, secret)
	return g, nil
}

// request hands an answered request to the sink the router set.
func (g *Engine) request(r apimirror.Request) {
	if f := g.sink.Load(); f != nil {
		(*f)(r)
	}
}

// SetRequestSink names where each answered request is reported. The router sets it, because the request log lives there.
func (g *Engine) SetRequestSink(f func(apimirror.Request)) { g.sink.Store(&f) }

// ServeHTTP answers a request that Declares accepted.
func (g *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.e.ServeHTTP(w, r) }

// Declares reports whether a spec route answers method on the escaped path.
func (g *Engine) Declares(method, path string) bool { return g.e.Declares(method, path) }

// Deliveries is the webhook.IngestNotifier that posts each applied delivery to the engine.
func (g *Engine) Deliveries() *Forwarder { return g.fwd }

// Start runs the engine's background jobs and the forwarder.
func (g *Engine) Start() {
	g.e.Start()
	g.fwd.start()
}

// Drain finishes the queued deliveries, then the engine's in-flight work. It reports false when either ran out of time.
func (g *Engine) Drain(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	fwdOK := g.fwd.drain(timeout)
	return g.e.Drain(time.Until(deadline)) && fwdOK
}

// Close releases the engine's database. Call it after Drain.
func (g *Engine) Close() error { return g.e.Close() }
