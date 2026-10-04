package sync

import (
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Resource kind constants used in cache_metadata.
const (
	// KindOrgRepos is a principal's org list-sync freshness marker; see docs/reveal-layer.md.
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
