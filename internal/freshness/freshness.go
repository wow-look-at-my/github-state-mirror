package freshness

import (
	"context"
	"fmt"
	"time"
)

// ResourceID uniquely identifies any cacheable resource.
type ResourceID struct {
	Kind string // e.g. "org_repos", "pr_files", "compare"
	Key  string // e.g. "my-org/my-repo/42"
}

func (r ResourceID) String() string {
	return fmt.Sprintf("%s:%s", r.Kind, r.Key)
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

// TriggerSource describes what caused a refresh.
type TriggerSource string

const (
	TriggerWebhook  TriggerSource = "webhook"
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
