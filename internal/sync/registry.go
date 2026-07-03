package sync

import (
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Resource kind constants used in cache_metadata.
const (
	// KindOrgRepos is a PRINCIPAL's org list-sync marker (actor = principal,
	// key = org login): freshness of that principal's grant set for the owner.
	// The fetch refreshes global truth as a side effect. (The /pulls list's
	// completeness marker lives in its own table, pulls_list_cache -- that
	// route absorbs the caller's own request rather than running a fetcher.)
	KindOrgRepos = "org_repos"
)

// RegisterAll wires all fetchers into the freshness.Manager.
func RegisterAll(mgr *freshness.Manager, gh *ghclient.Client, store *ghdata.Store) {
	mgr.RegisterFetcher(freshness.Policy{
		Kind:          KindOrgRepos,
		DefaultTTL:    6 * time.Hour,
		ErrorRetryMin: 1 * time.Minute,
	}, &OrgReposFetcher{gh: gh, store: store})
}
