package sync

import (
	"context"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// OrgReposFetcher runs one principal's owner LIST-SYNC: fetches repos + open
type OrgReposFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func (f *OrgReposFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	fetchStart := time.Now()
	principal := actor.FromContext(ctx)
	var data *ghclient.OrgData
	var err error
	if IsAppInstallationActor(principal) {
		data, err = f.gh.GetOwnerData(ctx, key)
	} else {
		data, err = f.gh.GetOrgData(ctx, key)
	}
	if err != nil {
		return freshness.RefreshResult{}, err
	}

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
