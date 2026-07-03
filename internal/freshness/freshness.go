package freshness

import (
	"context"
	"fmt"
	"time"
)

// GlobalActor marks a freshness row that tracks a piece of GLOBAL truth (any
// caller's fetch refreshes it for everyone) rather than one principal's own
// marker. It must be set explicitly on the ResourceID -- an empty Actor is
// filled from the request context (the caller's principal).
const GlobalActor = "global"

// ResourceID uniquely identifies any cacheable resource.
type ResourceID struct {
	Kind  string // e.g. "org_repos", "repo_pulls"
	Key   string // e.g. "my-org" or "my-org/my-repo"
	Actor string // a principal's marker, or GlobalActor for shared truth
}

func (r ResourceID) String() string {
	if r.Actor == "" {
		return fmt.Sprintf("%s:%s", r.Kind, r.Key)
	}
	return fmt.Sprintf("%s:%s@%s", r.Kind, r.Key, r.Actor)
}

// FetchState represents the lifecycle of a cached resource.
type FetchState string

const (
	StateUnknown  FetchState = "unknown"
	StateFresh    FetchState = "fresh"
	StateStale    FetchState = "stale"
	StateFetching FetchState = "fetching"
	StateError    FetchState = "error"
)

// Metadata holds the cache state for one resource.
type Metadata struct {
	ResourceID
	LastFetchedAt *time.Time
	LastChangedAt *time.Time
	ETag          string
	ExpiresAt     *time.Time
	State         FetchState
	ErrorMessage  string
	RetryAfter    *time.Time
}

// TriggerSource describes what caused a refresh (recorded in the refresh log).
// TriggerLazy is a caller's read finding the resource stale; TriggerPeriodic is
// the background refresher. (The old TriggerWebhook is gone: webhooks apply
// payload state directly and never fetch.)
type TriggerSource string

const (
	TriggerPeriodic TriggerSource = "periodic"
	TriggerLazy     TriggerSource = "lazy"
	TriggerManual   TriggerSource = "manual"
)

// RefreshResult is returned by a Fetcher after it completes.
type RefreshResult struct {
	RecordsChanged int
	NewETag        string
}

// Policy defines TTL and retry behavior for a resource kind.
type Policy struct {
	Kind          string
	DefaultTTL    time.Duration
	ErrorRetryMin time.Duration
	ErrorRetryMax time.Duration
}

// Fetcher is the interface that domain-specific sync code implements.
type Fetcher interface {
	Fetch(ctx context.Context, key string, etag string) (RefreshResult, error)
}
