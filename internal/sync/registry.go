package sync

import (
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Resource kind constants used in cache_metadata.
const (
	KindUser     = "user"
	KindUserOrgs = "user_orgs"
	KindOrgRepos = "org_repos"
	KindPRFiles  = "pr_files"
	KindCompare  = "compare"
)

// RegisterAll wires all fetchers into the freshness.Manager.
func RegisterAll(mgr *freshness.Manager, gh *ghclient.Client, store *ghdata.Store) {
	mgr.RegisterFetcher(freshness.Policy{
		Kind:          KindUser,
		DefaultTTL:    6 * time.Hour,
		ErrorRetryMin: 1 * time.Minute,
	}, &UserFetcher{gh: gh, store: store})

	mgr.RegisterFetcher(freshness.Policy{
		Kind:          KindUserOrgs,
		DefaultTTL:    6 * time.Hour,
		ErrorRetryMin: 1 * time.Minute,
	}, &UserOrgsFetcher{gh: gh, store: store})

	mgr.RegisterFetcher(freshness.Policy{
		Kind:          KindOrgRepos,
		DefaultTTL:    6 * time.Hour,
		ErrorRetryMin: 1 * time.Minute,
	}, &OrgReposFetcher{gh: gh, store: store})

	mgr.RegisterFetcher(freshness.Policy{
		Kind:          KindPRFiles,
		DefaultTTL:    1 * time.Hour,
		ErrorRetryMin: 30 * time.Second,
	}, &PRFilesFetcher{gh: gh, store: store})

	mgr.RegisterFetcher(freshness.Policy{
		Kind:          KindCompare,
		DefaultTTL:    30 * time.Minute,
		ErrorRetryMin: 30 * time.Second,
	}, &CompareFetcher{gh: gh, store: store})
}
