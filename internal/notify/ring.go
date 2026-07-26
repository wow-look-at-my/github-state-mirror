package notify

import (
	"sync"
	"time"
)

// Delivery outcomes recorded in the activity log.
const (
	// OutcomeDelivered: the subscriber answered 2xx.
	OutcomeDelivered = "delivered"
	// OutcomeFailed: every attempt failed (a terminal failure).
	OutcomeFailed = "failed"
	// OutcomeDisabled: a terminal failure that also tripped the auto-disable
	// threshold.
	OutcomeDisabled = "disabled"
	// OutcomeGated: the reveal gate blocked the notification (no public
	// visibility and no live grant — or the gate could not decide, which
	// fails closed).
	OutcomeGated = "gated"
)

// Attempt is one recorded delivery outcome for the admin view.
type Attempt struct {
	At             string `json:"at"` // RFC3339Nano UTC
	SubscriptionID string `json:"subscription_id"`
	Principal      string `json:"principal"`
	// PrincipalName is Principal's recorded display name. The notify package
	// never sets it — the admin handler joins it from actor_identities when
	// serving the activity view (display-only decoration).
	PrincipalName string `json:"principal_name,omitempty"`
	Event         string `json:"event"`
	Action        string `json:"action,omitempty"`
	Repo          string `json:"repo"` // owner/repo
	Disposition   string `json:"disposition"`
	Outcome       string `json:"outcome"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	Error         string `json:"error,omitempty"` // short error class, never a secret
	DurationMS    int64  `json:"duration_ms"`
}

// Counters are the cumulative since-restart totals.
type Counters struct {
	Delivered    int64 `json:"delivered"`
	Failed       int64 `json:"failed"`
	Gated        int64 `json:"gated"`
	AutoDisabled int64 `json:"auto_disabled"`
}

// activityRingCap bounds the recent-attempts ring.
const activityRingCap = 512

// activityLog is the in-memory, bounded record of recent delivery attempts
// plus cumulative counters — the requestLog stance: a live operational view,
// not an audit log. Deliberately unpersisted (it resets on restart); methods
// are safe for concurrent use and no-op on a nil receiver.
type activityLog struct {
	mu       sync.Mutex
	counters Counters
	recent   []Attempt // newest last, capped at activityRingCap
}

func (l *activityLog) record(a Attempt) {
	if l == nil {
		return
	}
	if a.At == "" {
		a.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch a.Outcome {
	case OutcomeDelivered:
		l.counters.Delivered++
	case OutcomeFailed:
		l.counters.Failed++
	case OutcomeDisabled:
		// A disabling failure is still a terminal failure.
		l.counters.Failed++
		l.counters.AutoDisabled++
	case OutcomeGated:
		l.counters.Gated++
	}
	l.recent = append(l.recent, a)
	if len(l.recent) > activityRingCap {
		l.recent = l.recent[len(l.recent)-activityRingCap:]
	}
}

// snapshot returns the counters and up to limit recent attempts, newest
// first. Nil-safe (returns zero values).
func (l *activityLog) snapshot(limit int) (Counters, []Attempt) {
	if l == nil {
		return Counters{}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.recent)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Attempt, 0, n)
	for i := len(l.recent) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, l.recent[i])
	}
	return l.counters, out
}
