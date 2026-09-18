package sync

import (
	"context"

	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// A create or delete delivery says a ref started or stopped existing. GitHub
// usually sends a push alongside it, and a push carries a tip this service can
// WRITE, so that path still does the applying. This handler is what remains
// when no push arrives: a ref created from an existing commit, and every
// delete, whose payload names the ref and carries no sha at all.
//
// A delete is the shape invalidation genuinely fits. The row must GO, and no
// value replaces it: serving a branch that stopped existing is the failure.
func (d *WebhookDispatcher) onRefLifecycle(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParseRefLifecyclePayload(event.Raw)
	if err != nil {
		return d.invalidateRepoOrg(ctx, event, "unparseable "+event.Type+" payload")
	}
	owner, repo, ref := payload.Owner, payload.Repo, payload.Ref
	if owner == "" || repo == "" {
		return d.invalidateRepoOrg(ctx, event, event.Type+" payload names no repository")
	}
	scope := owner + "/" + repo

	// Every spelling of the ref, because a cache row keys the REQUESTED ref
	// verbatim and a caller may ask by any of them.
	for _, spelling := range refSpellings(ref, payload.IsTag()) {
		flush("git ref cache", scope, d.store.InvalidateGitRefForRef(ctx, owner, repo, spelling))
		flush("contents cache", scope, d.store.InvalidateContentsForRef(ctx, owner, repo, spelling))
		flush("commits list cache", scope, d.store.InvalidateCommitsListForRef(ctx, owner, repo, spelling))
		flush("compare cache", scope, d.store.InvalidateCompareForRef(ctx, owner, repo, spelling))
		flush("commit CI cache", scope, d.store.InvalidateCommitCIForRef(ctx, owner, repo, spelling))
	}
	// The branches listing is a SET, and this delivery changed which refs are
	// in it. There is no tip to apply into a page, so the pages go.
	if !payload.IsTag() {
		flush("branches list cache", scope, d.store.InvalidateBranchesListCache(ctx, owner, repo))
	}
	// A matching-refs answer is a prefix search over the same refs.
	flush("matching refs cache", scope, d.store.InvalidateMatchingRefsCache(ctx, owner, repo))

	return outcome{
		disposition: webhook.DispInvalidated,
		detail:      event.Type + " " + payload.RefType + " " + ref + "; dropped every cached answer naming it",
	}
}
