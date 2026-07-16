package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// AccessChecker is the sliver of the ghdata store the reveal gate needs
// (ghdata.Store satisfies it as-is); an interface so tests can fake it. The
// gate reuses the reveal layer's NON-PROBING fast paths only: public
// visibility in global truth, or a live grant. There is deliberately no
// per-notification probe — no caller token exists at delivery time — and no
// deny-cache involvement; absence of public+grant simply gates the
// notification (fail closed).
type AccessChecker interface {
	GetRepoInsensitive(ctx context.Context, owner, name string) (dbgen.Repo, error)
	HasGrant(ctx context.Context, principal, owner, repo string, now time.Time) (bool, error)
}

// Tunable defaults (see Config).
const (
	defaultMaxConcurrent  = 8
	defaultAttempts       = 3
	defaultAttemptTimeout = 8 * time.Second
	defaultDisableAfter   = 10
	defaultUserAgent      = "github-state-mirror-notifier"

	// fanOutTimeout bounds one delivery's subscription listing + reveal
	// gating (local SQLite reads).
	fanOutTimeout = 30 * time.Second
	// storeOpTimeout bounds the per-delivery outcome writes.
	storeOpTimeout = 5 * time.Second
	// maxErrorLen caps the stored last_error string.
	maxErrorLen = 200
)

func defaultBackoff() []time.Duration { return []time.Duration{1 * time.Second, 4 * time.Second} }

// Config configures a Notifier. Zero fields take the documented defaults.
type Config struct {
	Store  *Store
	Access AccessChecker
	// HTTPClient posts the notifications (per-attempt timeouts are applied via
	// request contexts). Default: a plain &http.Client{}.
	HTTPClient *http.Client
	// MaxConcurrent bounds concurrent subscriber POSTs (default 8).
	MaxConcurrent int
	// Attempts per delivery (default 3).
	Attempts int
	// AttemptTimeout bounds each POST (default 8s).
	AttemptTimeout time.Duration
	// Backoff between attempts (default 1s then 4s; the last entry repeats).
	Backoff []time.Duration
	// DisableAfter is the consecutive-terminal-failure count that
	// auto-disables a subscription (default 10).
	DisableAfter int
	// UserAgent identifies the mirror to subscribers.
	UserAgent string
	// Timeline records every outbound delivery attempt (real per-attempt
	// duration, status, terminal flag) onto the dashboard's Timeline chart.
	// Nil-safe: nil records nothing.
	Timeline *reqtimeline.Recorder
}

// Notifier fans a post-ingest event out to matching, authorized
// subscriptions. Delivery work is DETACHED like the freshness fetches: it
// runs on context.Background-derived contexts with bounded timeouts, tracked
// in a WaitGroup, and Drain waits it out at shutdown BEFORE the DBs close.
// All methods are nil-receiver-safe, so wiring may pass a nil notifier to
// keep the feature inert.
type Notifier struct {
	store          *Store
	access         AccessChecker
	httpc          *http.Client
	log            *activityLog
	sem            chan struct{}
	attempts       int
	attemptTimeout time.Duration
	backoff        []time.Duration
	disableAfter   int
	userAgent      string
	timeline       *reqtimeline.Recorder

	mu       sync.Mutex // guards stopping + the Add-vs-Wait race
	stopping bool
	stop     chan struct{}
	wg       sync.WaitGroup
}

// New builds a Notifier from cfg, applying defaults to zero fields.
func New(cfg Config) *Notifier {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = defaultAttempts
	}
	if cfg.AttemptTimeout <= 0 {
		cfg.AttemptTimeout = defaultAttemptTimeout
	}
	if len(cfg.Backoff) == 0 {
		cfg.Backoff = defaultBackoff()
	}
	if cfg.DisableAfter <= 0 {
		cfg.DisableAfter = defaultDisableAfter
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	return &Notifier{
		store:          cfg.Store,
		access:         cfg.Access,
		httpc:          cfg.HTTPClient,
		log:            &activityLog{},
		sem:            make(chan struct{}, cfg.MaxConcurrent),
		attempts:       cfg.Attempts,
		attemptTimeout: cfg.AttemptTimeout,
		backoff:        cfg.Backoff,
		disableAfter:   cfg.DisableAfter,
		userAgent:      cfg.UserAgent,
		timeline:       cfg.Timeline,
		stop:           make(chan struct{}),
	}
}

// Store exposes the subscriptions store (nil on a nil Notifier) for the CRUD
// API layer.
func (n *Notifier) Store() *Store {
	if n == nil {
		return nil
	}
	return n.store
}

// Activity returns the cumulative counters and up to limit recent delivery
// attempts, newest first. Nil-safe.
func (n *Notifier) Activity(limit int) (Counters, []Attempt) {
	if n == nil {
		return Counters{}, nil
	}
	return n.log.snapshot(limit)
}

// AllSubscriptions returns every subscription (operator view). Nil-safe.
func (n *Notifier) AllSubscriptions(ctx context.Context) ([]Subscription, error) {
	if n == nil || n.store == nil {
		return nil, nil
	}
	return n.store.ListAll(ctx)
}

// NotifyIngest implements webhook.IngestNotifier: it enqueues subscriber
// fan-out for a dispatched delivery and returns immediately — the GitHub
// webhook response never waits on subscriber POSTs. Only the applied and
// invalidated dispositions notify, and only deliveries that resolve to a
// single owner/repo (repo-less events like installation are skipped).
func (n *Notifier) NotifyIngest(event webhook.Event, result webhook.DispatchResult, ingestedAt time.Time) {
	if n == nil || n.store == nil {
		return
	}
	if result.Disposition != webhook.DispApplied && result.Disposition != webhook.DispInvalidated {
		return
	}
	if event.RepoOwner() == "" || event.RepoName() == "" {
		return
	}
	if !n.track() {
		return
	}
	go n.fanOut(event, result.Disposition, ingestedAt)
}

// Drain stops new deliveries and retries promptly (pending backoff sleeps are
// cut) and waits for in-flight work to finish, or the timeout to elapse. Call
// it during shutdown BEFORE closing the databases. Returns true when fully
// drained. Nil-safe.
func (n *Notifier) Drain(timeout time.Duration) bool {
	if n == nil {
		return true
	}
	n.mu.Lock()
	if !n.stopping {
		n.stopping = true
		close(n.stop)
	}
	n.mu.Unlock()
	return n.awaitQuiescence(timeout)
}

// Flush waits for in-flight deliveries (including retries) WITHOUT stopping
// anything — the deterministic wait tests use. Nil-safe.
func (n *Notifier) Flush(timeout time.Duration) bool {
	if n == nil {
		return true
	}
	return n.awaitQuiescence(timeout)
}

func (n *Notifier) awaitQuiescence(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// track registers one unit of in-flight work, refusing (false) once the
// notifier is stopping. The mutex closes the WaitGroup Add-after-Wait race:
// stopping is flipped under the same lock, so no Add can slip in after Drain
// began waiting on a drained group.
func (n *Notifier) track() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopping {
		return false
	}
	n.wg.Add(1)
	return true
}

// fanOut matches one ingested delivery against every active subscription and
// spawns a delivery per authorized match. Matching (active + event filter +
// repo filter) and the reveal gate run per delivery, against live state.
func (n *Notifier) fanOut(event webhook.Event, disposition string, ingestedAt time.Time) {
	defer n.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), fanOutTimeout)
	defer cancel()

	subs, err := n.store.ListEnabled(ctx)
	if err != nil {
		slog.Warn("notify: list subscriptions failed", "error", err)
		return
	}
	owner, repo := event.RepoOwner(), event.RepoName()
	ids := extractIdentifiers(event)

	for _, sub := range subs {
		if !sub.MatchesEvent(event.Type) || !sub.MatchesRepo(owner, repo) {
			continue
		}
		if !n.revealAllowed(ctx, sub.Principal, owner, repo) {
			n.log.record(Attempt{
				SubscriptionID: sub.ID, Principal: sub.Principal,
				Event: event.Type, Action: event.Action, Repo: owner + "/" + repo,
				Disposition: disposition, Outcome: OutcomeGated,
			})
			continue
		}
		note := Notification{
			MirrorDelivery: NewDeliveryID(),
			SubscriptionID: sub.ID,
			GitHubDelivery: event.DeliveryID,
			Event:          event.Type,
			Action:         event.Action,
			Owner:          owner,
			Repo:           repo,
			RepoFullName:   owner + "/" + repo,
			PRNumber:       ids.prNumber,
			Ref:            ids.ref,
			SHA:            ids.sha,
			Disposition:    disposition,
			IngestedAt:     rfc3339(ingestedAt),
		}
		if !n.track() {
			return // stopping; skip the remaining fan-out too
		}
		go n.deliver(sub, note)
	}
}

// revealAllowed applies the reveal layer's non-probing fast paths: the repo
// is public in global truth, or the principal holds a live grant. Unknown or
// absent visibility reads as private (grant required), and a store error
// gates the notification — a private repo's activity must never reach an
// unauthorized principal, so every failure mode fails closed.
func (n *Notifier) revealAllowed(ctx context.Context, principal, owner, repo string) bool {
	if n.access == nil {
		return false
	}
	if repoRow, err := n.access.GetRepoInsensitive(ctx, owner, repo); err == nil &&
		repoRow.Visibility == ghdata.VisibilityPublic {
		return true
	}
	if principal == "" {
		return false
	}
	ok, err := n.access.HasGrant(ctx, principal, owner, repo, time.Now())
	if err != nil {
		slog.Warn("notify: grant lookup failed; gating notification",
			"repo", owner+"/"+repo, "error", err)
		return false
	}
	return ok
}

// deliver POSTs one notification with bounded retries, recording the outcome.
// A stop/drain signal cuts pending backoff sleeps; a delivery abandoned by
// shutdown records nothing (it neither succeeded nor terminally failed, and
// counting it toward auto-disable would be unfair to the subscriber).
func (n *Notifier) deliver(sub Subscription, note Notification) {
	defer n.wg.Done()
	select {
	case n.sem <- struct{}{}:
	case <-n.stop:
		return
	}
	defer func() { <-n.sem }()

	start := time.Now()
	var lastStatus int
	var lastErr string
	for attempt := 1; attempt <= n.attempts; attempt++ {
		if attempt > 1 {
			if !n.sleep(n.backoffFor(attempt)) {
				return // shutdown cut the backoff; abandon
			}
		}
		attemptStart := time.Now()
		status, err := n.post(sub, note)
		ok := err == nil && status >= 200 && status < 300
		// Every attempt is a real outbound request: chart it, with its real
		// duration, whether it succeeded, failed, or will be retried.
		disp := "failed"
		if ok {
			disp = "delivered"
		}
		n.timeline.RecordNotify(attemptStart, time.Since(attemptStart), subscriptionHost(sub.URL), status, attempt, ok || attempt == n.attempts, disp)
		if ok {
			n.recordDelivered(sub, note, status, time.Since(start))
			return
		}
		if err != nil {
			lastStatus = 0
			lastErr = shortError(errClass(err) + ": " + err.Error())
		} else {
			lastStatus = status
			lastErr = fmt.Sprintf("http %d", status)
		}
	}
	n.recordTerminalFailure(sub, note, lastStatus, lastErr, time.Since(start))
}

// subscriptionHost is the display target for a delivery attempt: the URL's
// host only — never the full URL, whose path/query is subscriber
// configuration that could embed credentials.
func subscriptionHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "(invalid url)"
}

// backoffFor returns the sleep before the given attempt (attempt >= 2); the
// last configured backoff repeats.
func (n *Notifier) backoffFor(attempt int) time.Duration {
	i := attempt - 2
	if i >= len(n.backoff) {
		i = len(n.backoff) - 1
	}
	return n.backoff[i]
}

// sleep waits for d unless the stop signal cuts it short (false).
func (n *Notifier) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-n.stop:
		return false
	}
}

// post sends one signed attempt. SentAt is stamped per attempt so the
// signature always covers the exact bytes sent.
func (n *Notifier) post(sub Subscription, note Notification) (int, error) {
	note.SentAt = rfc3339(time.Now())
	body, err := json.Marshal(note)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), n.attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", n.userAgent)
	req.Header.Set("X-Mirror-Event", note.Event)
	req.Header.Set("X-Mirror-Delivery", note.MirrorDelivery)
	req.Header.Set("X-Hub-Signature-256", SignBody(sub.Secret, body))
	resp, err := n.httpc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func (n *Notifier) recordDelivered(sub Subscription, note Notification, status int, dur time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), storeOpTimeout)
	defer cancel()
	if err := n.store.RecordSuccess(ctx, sub.ID, time.Now()); err != nil {
		slog.Warn("notify: record success failed", "subscription", sub.ID, "error", err)
	}
	n.log.record(Attempt{
		SubscriptionID: sub.ID, Principal: sub.Principal,
		Event: note.Event, Action: note.Action, Repo: note.RepoFullName,
		Disposition: note.Disposition, Outcome: OutcomeDelivered,
		HTTPStatus: status, DurationMS: dur.Milliseconds(),
	})
}

func (n *Notifier) recordTerminalFailure(sub Subscription, note Notification, status int, errMsg string, dur time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), storeOpTimeout)
	defer cancel()
	failures, disabled, err := n.store.RecordFailure(ctx, sub.ID, errMsg, n.disableAfter, time.Now())
	if err != nil {
		slog.Warn("notify: record failure failed", "subscription", sub.ID, "error", err)
	}
	outcome := OutcomeFailed
	if disabled {
		outcome = OutcomeDisabled
		// The URL carries no userinfo (validation rejects it) and the secret
		// is never logged.
		slog.Warn("notify: subscription auto-disabled after consecutive delivery failures",
			"subscription", sub.ID, "failures", failures, "error", errMsg)
	}
	n.log.record(Attempt{
		SubscriptionID: sub.ID, Principal: sub.Principal,
		Event: note.Event, Action: note.Action, Repo: note.RepoFullName,
		Disposition: note.Disposition, Outcome: outcome,
		HTTPStatus: status, Error: errMsg, DurationMS: dur.Milliseconds(),
	})
}

// SignBody computes the X-Hub-Signature-256 header value for a delivery body:
// "sha256=" + hex(HMAC-SHA256(secret, body)) — GitHub's exact scheme, so
// subscribers reuse their existing webhook verification code.
func SignBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// errClass folds a transport error into a short class for logs and the
// activity ring.
func errClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "network"
}

// shortError caps an error string for storage (last_error stays SHORT).
func shortError(s string) string {
	if len(s) > maxErrorLen {
		return s[:maxErrorLen]
	}
	return s
}
