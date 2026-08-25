package ghdata

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// A `status` delivery carries the whole status it announces, so the two

// CommitStatusUpdate is one `status` delivery's account of one commit status,
// in GitHub's own spelling of every field the stored documents carry.
type CommitStatusUpdate struct {
	SHA         string
	Context     string
	State       string
	Description *string
	TargetURL   *string
	CreatedAt   string
	UpdatedAt   string
}

// storedCommitStatusItem and storedCombinedStatus mirror the kind-"status"
// document; field order is wire order and must match the api package's
type storedCommitStatusItem struct {
	Context     string  `json:"context"`
	State       string  `json:"state"`
	Description *string `json:"description"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

type storedCombinedStatus struct {
	State      string                   `json:"state"`
	SHA        string                   `json:"sha"`
	TotalCount int64                    `json:"total_count"`
	Statuses   []storedCommitStatusItem `json:"statuses"`
}

// storedStatusListItem is one entry of the kind-"statuses_list" document (a
// bare array). Same rule as above: field order is wire order.
type storedStatusListItem struct {
	Context     string  `json:"context"`
	State       string  `json:"state"`
	Description *string `json:"description"`
	TargetURL   *string `json:"target_url"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// SettleCommitCIFromStatus lands a `status` delivery on the cached commit-CI
// documents it moves: each is rewritten from the payload where the stored
// document can provably hold the result, and dropped where it cannot.
//
// Which rows it reaches is decided by what the payload can identify, not by
// which spellings the caller named:
//
//   - kind "status" documents state the sha they resolved to, so EVERY
//     spelling describing this commit is recognizable from its own contents --
//     a branch-form row whose document names another sha is left alone,
//     because this status did not move it.
//   - kind "statuses_list" documents are bare arrays with no sha in them, so
//     only the sha-form row is provably about this commit. The branch
//     spellings the payload names are flushed, as before.
//
// The check-runs kind is not touched at all: a status never appears in a
// check-runs listing (statuses and checks are separate surfaces upstream).
func (s *Store) SettleCommitCIFromStatus(ctx context.Context, owner, repo string, branchRefs []string, up CommitStatusUpdate, now time.Time, ttl time.Duration) error {
	if !IsFullHexSHA(up.SHA) || up.Context == "" || up.State == "" {
		return nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)

	combined, err := s.q.ListCommitCICacheByRepoKind(ctx, dbgen.ListCommitCICacheByRepoKindParams{
		Owner: ownerKey, Repo: repoKey, Kind: CommitCIKindStatus,
	})
	if err != nil {
		return err
	}
	for _, row := range combined {
		if !combinedStatusDescribes(row.Doc, up.SHA) {
			continue // a different commit: this status did not move it
		}
		patched, ok := patchCombinedStatus(row.Doc, row.PerPage, row.Page, up)
		if !ok {
			if derr := s.deleteCommitCIRow(ctx, ownerKey, repoKey, row); derr != nil {
				return derr
			}
			continue
		}
		if perr := s.PutCachedCommitCI(ctx, CachedCommitCI{
			// Status 200: combinedStatusDescribes only matched a sha-named doc, never a 404 tombstone.
			Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Kind: row.Kind, Status: 200, Doc: patched,
		}, int(row.PerPage), int(row.Page), now, ttl); perr != nil {
			return perr
		}
	}

	lists, err := s.q.ListCommitCICacheByRepoKind(ctx, dbgen.ListCommitCICacheByRepoKindParams{
		Owner: ownerKey, Repo: repoKey, Kind: CommitCIKindStatusesList,
	})
	if err != nil {
		return err
	}
	for _, row := range lists {
		if row.Ref != up.SHA {
			continue // handled by the branch flush below, if the payload named it
		}
		patched, ok := patchStatusesList(row.Doc, row.PerPage, row.Page, up)
		if !ok {
			if derr := s.deleteCommitCIRow(ctx, ownerKey, repoKey, row); derr != nil {
				return derr
			}
			continue
		}
		if perr := s.PutCachedCommitCI(ctx, CachedCommitCI{
			// Status 200: a 404 tombstone (an object, not an array) fails to unmarshal into patchStatusesList's input.
			Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Kind: row.Kind, Status: 200, Doc: patched,
		}, int(row.PerPage), int(row.Page), now, ttl); perr != nil {
			return perr
		}
	}

	for _, ref := range branchRefs {
		if ref == up.SHA {
			continue
		}
		if err := s.q.DeleteCommitCICacheForRefKind(ctx, dbgen.DeleteCommitCICacheForRefKindParams{
			Owner: ownerKey, Repo: repoKey, Ref: ref, Kind: CommitCIKindStatusesList,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ApplyCheckRunToCommitCI rewrites a run's entry in every cached check-runs
// page that already lists it, reporting false when the caller should flush
// instead. See "The check-run rewrite" in docs/webhooks/invalidations.md for
// the ordering measurement that makes an in-place rewrite safe.
func (s *Store) ApplyCheckRunToCommitCI(ctx context.Context, owner, repo string, run StoredCheckRun, now time.Time, ttl time.Duration) (bool, error) {
	if run.ID <= 0 || run.Status == "" || !IsFullHexSHA(run.HeadSHA) {
		return false, nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	rows, err := s.q.ListCommitCICacheByRepoKind(ctx, dbgen.ListCommitCICacheByRepoKindParams{
		Owner: ownerKey, Repo: repoKey, Kind: CommitCIKindCheckRuns,
	})
	if err != nil {
		return false, err
	}
	applied := false
	for _, row := range rows {
		patched, ok := patchCheckRunsPage(row.Doc, row.PerPage, row.Page, run)
		if !ok {
			if !checkRunsPageDescribes(row.Doc, run.HeadSHA) {
				continue // a page about another commit: this run did not move it
			}
			if derr := s.deleteCommitCIRow(ctx, ownerKey, repoKey, row); derr != nil {
				return false, derr
			}
			continue
		}
		if perr := s.PutCachedCommitCI(ctx, CachedCommitCI{
			// Status 200: patchCheckRunsPage only succeeds against a page that already lists this run.
			Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Kind: row.Kind, Status: 200, Doc: patched,
		}, int(row.PerPage), int(row.Page), now, ttl); perr != nil {
			return false, perr
		}
		applied = true
	}
	return applied, nil
}

// checkRunsPageDescribes reports whether a page is about this commit, from its entries' own head_sha (the page carries none).
func checkRunsPageDescribes(doc, sha string) bool {
	var page StoredCheckRunsPage
	if err := json.Unmarshal([]byte(doc), &page); err != nil || len(page.CheckRuns) == 0 {
		return false
	}
	for _, cr := range page.CheckRuns {
		if cr.HeadSHA != sha {
			return false
		}
	}
	return true
}

// patchCheckRunsPage rewrites a stored check-runs page from the delivery,
// replacing the run's entry where it stands.
//
// Reports false -- the caller drops the row instead -- for a page other than
// the first, a page that does not hold every run (total_count above its own
// length), a run the page does not list (a new check run is a membership
// change only a fetch settles), and the one transition whose POSITION could
// move: a queued run gaining a `started_at`. The listing is ordered by id, so
// an update cannot move an entry -- but a queued run has no start time at all,
// and refusing that single case costs one flush instead of resting the whole
// rewrite on the ordering measurement holding for a field that was null.
func patchCheckRunsPage(doc string, perPage, page int64, run StoredCheckRun) (string, bool) {
	if page != 1 {
		return "", false
	}
	var stored StoredCheckRunsPage
	if err := json.Unmarshal([]byte(doc), &stored); err != nil {
		return "", false
	}
	if stored.TotalCount != int64(len(stored.CheckRuns)) || int64(len(stored.CheckRuns)) > perPage {
		return "", false
	}
	found := false
	for i := range stored.CheckRuns {
		if stored.CheckRuns[i].ID != run.ID {
			continue
		}
		if stored.CheckRuns[i].StartedAt == nil && run.StartedAt != nil {
			return "", false
		}
		stored.CheckRuns[i] = run
		found = true
	}
	if !found {
		return "", false
	}
	rendered, err := MarshalCacheDoc(stored)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}

// InvalidateCommitCIForRefKind drops one ref spelling's snapshots of ONE kind.
// A check delivery uses it for the check-runs kind: a check run never appears
// in a commit's statuses, so flushing those rows too would re-fetch answers
// the delivery cannot have changed.
func (s *Store) InvalidateCommitCIForRefKind(ctx context.Context, owner, repo, ref, kind string) error {
	return s.q.DeleteCommitCICacheForRefKind(ctx, dbgen.DeleteCommitCICacheForRefKindParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Ref: ref, Kind: kind,
	})
}

func (s *Store) deleteCommitCIRow(ctx context.Context, owner, repo string, row dbgen.CommitCiCache) error {
	return s.q.DeleteCommitCICacheForRefKind(ctx, dbgen.DeleteCommitCICacheForRefKindParams{
		Owner: owner, Repo: repo, Ref: row.Ref, Kind: row.Kind,
	})
}

// combinedStatusDescribes reports whether a stored document's own resolved sha matches, so a branch-form row is usable.
func combinedStatusDescribes(doc, sha string) bool {
	var d struct {
		SHA string `json:"sha"`
	}
	return json.Unmarshal([]byte(doc), &d) == nil && d.SHA == sha
}

// patchCombinedStatus rewrites a stored combined status from the delivery: the
// context's previous entry is REPLACED (the endpoint holds the latest per
// context) and the new one lands at the end, where the array's oldest-first
// order puts the newest status.
//
// Reports false -- the caller drops the row instead -- whenever the result is
// not provably what a fetch would return: a page other than the first, a page
// that does not hold every context (total_count above its own length), a page
// with no room for a new context, or a status that is not the newest on the
// commit and so does not belong at the end.
func patchCombinedStatus(doc string, perPage, page int64, up CommitStatusUpdate) (string, bool) {
	if page != 1 {
		return "", false
	}
	var d storedCombinedStatus
	if err := json.Unmarshal([]byte(doc), &d); err != nil || d.SHA != up.SHA {
		return "", false
	}
	if d.TotalCount != int64(len(d.Statuses)) {
		return "", false // a later page holds contexts this rewrite cannot see
	}
	kept := make([]storedCommitStatusItem, 0, len(d.Statuses)+1)
	for _, item := range d.Statuses {
		// Not the newest on the commit: where it belongs is unknown, so refuse.
		if !notOlder(up.CreatedAt, item.CreatedAt) {
			return "", false
		}
		if item.Context == up.Context {
			continue // superseded: statuses are append-only, so this is a new object
		}
		kept = append(kept, item)
	}
	kept = append(kept, storedCommitStatusItem{
		Context: up.Context, State: up.State, Description: up.Description,
		CreatedAt: up.CreatedAt, UpdatedAt: up.UpdatedAt,
	})
	if int64(len(kept)) > perPage {
		return "", false // the answer no longer fits on this page
	}
	d.Statuses = kept
	d.TotalCount = int64(len(kept))
	d.State = combinedStatusRollup(kept)
	rendered, err := MarshalCacheDoc(d)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}

// patchStatusesList rewrites a stored raw statuses list from the delivery. The
// list is append-only history, newest-first, so the new status is PREPENDED --
// the context's older entries stay, exactly as upstream keeps them. A page
// that overflows drops its oldest entry, which is precisely where page 2
// begins; every other page of this ref is dropped by the caller's fallback,
// since their contents shift.
//
// Reports false for a page other than the first, an unreadable document, or a
// status older than the one already at the head (its position is then unknown).
func patchStatusesList(doc string, perPage, page int64, up CommitStatusUpdate) (string, bool) {
	if page != 1 || perPage < 1 {
		return "", false
	}
	var items []storedStatusListItem
	if err := json.Unmarshal([]byte(doc), &items); err != nil {
		return "", false
	}
	for _, item := range items {
		if item.Context == up.Context && item.CreatedAt == up.CreatedAt {
			return doc, true // already absorbed (a redelivery); rewriting would duplicate it
		}
	}
	if len(items) > 0 && !notOlder(up.CreatedAt, items[0].CreatedAt) {
		return "", false
	}
	out := make([]storedStatusListItem, 0, len(items)+1)
	out = append(out, storedStatusListItem{
		Context: up.Context, State: up.State, Description: up.Description,
		TargetURL: up.TargetURL, CreatedAt: up.CreatedAt, UpdatedAt: up.UpdatedAt,
	})
	out = append(out, items...)
	if int64(len(out)) > perPage {
		out = out[:perPage]
	}
	rendered, err := MarshalCacheDoc(out)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}

// combinedStatusRollup is GitHub's documented combined state: "failure if any
// of the contexts report as error or failure; pending if there are no statuses
// or a context is pending; success if the latest status for all contexts is
// success" (REST docs, commits/statuses).
func combinedStatusRollup(items []storedCommitStatusItem) string {
	if len(items) == 0 {
		return "pending"
	}
	pending := false
	for _, item := range items {
		switch item.State {
		case "failure", "error":
			return "failure"
		case "pending":
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	return "success"
}

// notOlder reports a at or after b; an unparseable timestamp on either side answers false rather than guess.
func notOlder(a, b string) bool {
	at, err := time.Parse(time.RFC3339, a)
	if err != nil {
		return false
	}
	bt, err := time.Parse(time.RFC3339, b)
	if err != nil {
		return false
	}
	return !at.Before(bt)
}
