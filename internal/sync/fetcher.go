package sync

import (
	"context"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// OrgReposFetcher runs one principal's org LIST-SYNC: it fetches all repos +
// open PRs for an org via GraphQL WITH THE PRINCIPAL'S OWN TOKEN, merges the
// snapshot into global truth (upsert + guarded reconcile -- see
// Store.SyncOrgTruth), and replace-syncs the principal's access grants (every
// repo GitHub returned to them = proof they may read it). Key: org login;
// the freshness marker is per principal (the actor in context).
type OrgReposFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func (f *OrgReposFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	fetchStart := time.Now()
	data, err := f.gh.GetOrgData(ctx, key)
	if err != nil {
		return freshness.RefreshResult{}, err
	}

	principal := actor.FromContext(ctx)
	sync := ghdata.OrgSyncData{
		Repos:      data.Repos,
		PRsByRepo:  data.PRsByRepo,
		LabelsByPR: data.LabelsByPR,
	}
	if err := f.store.SyncOrgTruth(ctx, key, sync, principal, fetchStart, time.Now()); err != nil {
		return freshness.RefreshResult{}, err
	}

	changed := len(data.Repos)
	for _, prs := range data.PRsByRepo {
		changed += len(prs)
	}
	return freshness.RefreshResult{RecordsChanged: changed}, nil
}
