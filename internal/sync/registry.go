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
	// The fetch refreshes global truth as a side effect.
	KindOrgRepos = "org_repos"
	// KindRepoPulls is GLOBAL truth freshness for one repo's open-PR list
	// (actor = freshness.GlobalActor, key = "owner/repo"): any principal's
	// fetch refreshes it for everyone.
	KindRepoPulls = "repo_pulls"
)

// RegisterAll wires all fetchers into the freshness.Manager.
func RegisterAll(mgr *freshness.Manager, gh *ghclient.Client, store *ghdata.Store) {
	mgr.RegisterFetcher(freshness.Policy{
		Kind:          KindOrgRepos,
		DefaultTTL:    6 * time.Hour,
		ErrorRetryMin: 1 * time.Minute,
	}, &OrgReposFetcher{gh: gh, store: store})

	mgr.RegisterFetcher(freshness.Policy{
		// Webhooks keep the open-PR set live; the TTL only bounds how long a
		// missed webhook could mislead the /pulls list.
		Kind:          KindRepoPulls,
		DefaultTTL:    30 * time.Minute,
		ErrorRetryMin: 30 * time.Second,
	}, &RepoPullsFetcher{gh: gh, store: store})
}
