// A `status` delivery carries the whole status it announces.

package ghdata

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// A `status` delivery carries the whole status it announces.

// CommitStatusUpdate is `status` delivery's account of commit status,
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

// storedStatusListItem is entry of the kind-"statuses_list" document (a
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
// documents it moves. Each is rewritten from the payload where the stored
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
func (s *Store) SettleCommitCIFromStatus(ctx context.Context, owner, repo string, branchRefs []string, up CommitStatusUpdate, now time.Time, ttl time.Duration) error {
	if !IsFullHexSHA(up.SHA) || up.Context == "" || up.State == "" {
		return nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)

	combined, err := s.qx(ctx).ListCommitCICacheByRepoKind(ctx, dbgen.ListCommitCICacheByRepoKindParams{
		Owner: ownerKey, Repo: repoKey, Kind: CommitCIKindStatus,
	})
	if err != nil {
		return err
	}
	for _, row := range combined {
		if !combinedStatusDescribes(row.Doc, up.SHA) {
			if row.Ref == up.SHA {
				if derr := s.deleteCommitCIRow(ctx, ownerKey, repoKey, row); derr != nil {
					return derr
				}
			}
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
			// Status: combinedStatusDescribes only matched a sha-named doc, never a tombstone.
			Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Kind: row.Kind, Status: 200, Doc: patched,
		}, int(row.PerPage), int(row.Page), now, ttl); perr != nil {
			return perr
		}
	}

	lists, err := s.qx(ctx).ListCommitCICacheByRepoKind(ctx, dbgen.ListCommitCICacheByRepoKindParams{
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
			// Status: a tombstone (an object, not an array) fails to unmarshal into patchStatusesList's input.
			Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Kind: row.Kind, Status: 200, Doc: patched,
		}, int(row.PerPage), int(row.Page), now, ttl); perr != nil {
			return perr
		}
	}

	for _, ref := range branchRefs {
		if ref == up.SHA {
			continue
		}
		if err := s.qx(ctx).DeleteCommitCICacheForRefKind(ctx, dbgen.DeleteCommitCICacheForRefKindParams{
			Owner: ownerKey, Repo: repoKey, Ref: ref, Kind: CommitCIKindStatusesList,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ApplyCheckRunToCommitCI lands a run on every cached check-runs page about
// its commit. This is replacing its entry, or adding a run the page does
// not list yet -- and reports false when the caller should flush instead.
// See "The check-run rewrite" in docs/webhooks/invalidations.md for the
// ordering measurement that makes each placement a fact rather than a hope.
func (s *Store) ApplyCheckRunToCommitCI(ctx context.Context, owner, repo string, run StoredCheckRun, now time.Time, ttl time.Duration) (bool, error) {
	if run.ID <= 0 || run.Status == "" || !IsFullHexSHA(run.HeadSHA) {
		return false, nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	rows, err := s.qx(ctx).ListCommitCICacheByRepoKind(ctx, dbgen.ListCommitCICacheByRepoKindParams{
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
			// Status: patchCheckRunsPage only succeeds against a page of this run's own commit.
			Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Kind: row.Kind, Status: 200, Doc: patched,
		}, int(row.PerPage), int(row.Page), now, ttl); perr != nil {
			return false, perr
		}
		applied = true
	}
	return applied, nil
}

// DropCheckRunPagesContradictedByRun deletes every cached check-runs page of
// headSHA that still lists a job of this Actions run as unfinished. The caller
// calls it only for a completed run, and a completed run has no unfinished job.
// Such a page lost a check_run delivery, and no later delivery corrects it.
// The run payload does not state each job's conclusion, so a flush is the only
// repair. It returns the count of pages it deleted.
func (s *Store) DropCheckRunPagesContradictedByRun(ctx context.Context, owner, repo, headSHA string, runID int64) (int, error) {
	if runID <= 0 || !IsFullHexSHA(headSHA) {
		return 0, nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	rows, err := s.qx(ctx).ListCommitCICacheByRepoKind(ctx, dbgen.ListCommitCICacheByRepoKindParams{
		Owner: ownerKey, Repo: repoKey, Kind: CommitCIKindCheckRuns,
	})
	if err != nil {
		return 0, err
	}
	jobPath := fmt.Sprintf("/actions/runs/%d/job/", runID)
	dropped := 0
	for _, row := range rows {
		var page StoredCheckRunsPage
		if err := json.Unmarshal([]byte(row.Doc), &page); err != nil || !pageDescribesSHA(&page, headSHA) {
			continue
		}
		if !listsUnfinishedJobOf(&page, jobPath) {
			continue
		}
		if err := s.deleteCommitCIRow(ctx, ownerKey, repoKey, row); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}

// listsUnfinishedJobOf reports whether the page holds a run that is not
// completed and whose URL names a job under jobPath.
func listsUnfinishedJobOf(page *StoredCheckRunsPage, jobPath string) bool {
	for _, cr := range page.CheckRuns {
		if cr.Status == "completed" {
			continue
		}
		for _, u := range []*string{cr.DetailsURL, cr.HTMLURL} {
			if u != nil && strings.Contains(*u, jobPath) {
				return true
			}
		}
	}
	return false
}

// checkRunsPageDescribes reports whether a stored page is about.
func checkRunsPageDescribes(doc, sha string) bool {
	var page StoredCheckRunsPage
	if err := json.Unmarshal([]byte(doc), &page); err != nil {
		return false
	}
	return pageDescribesSHA(&page, sha)
}

// pageDescribesSHA reports whether a page is about this commit, from its entries' own head_sha (the page carries none).
func pageDescribesSHA(page *StoredCheckRunsPage, sha string) bool {
	if len(page.CheckRuns) == 0 {
		return false
	}
	for _, cr := range page.CheckRuns {
		if cr.HeadSHA != sha {
			return false
		}
	}
	return true
}

// patchCheckRunsPage rewrites a stored check-runs page from the delivery. It
// replaces the run's entry where it stands, or INSERTS a run the page does not
// list yet at its place in the id order.
//
// The insert is what keeps a finishing CI run out of the flush path. The last
// deliveries of a run are a new job's creation and a queued job's start.
// Dropping the row for either sends the next reader to GitHub at the exact
// moment GitHub's own listing is furthest behind these deliveries. The
// refetch then stores a view OLDER than what the payloads already carried,
// and no further delivery for that commit ever arrives to correct it.
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
		// A queued run gaining a `started_at` is replaced like any other transition: the listing sorts on the id, which this delivery does not change.
		stored.CheckRuns[i] = run
		found = true
	}
	if !found && !insertCheckRun(&stored, perPage, run) {
		return "", false
	}
	rendered, err := MarshalCacheDoc(stored)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}

// insertCheckRun adds a run the page does not list yet. That insertCheckRun is
// at its place in the id-descending order the listing uses, and reports false
// when the page cannot take it. A creation names the commit the run belongs
// to, so membership GROWS from the payload. What no delivery states is that a
// run went away, which is what the TTL still bounds.
func insertCheckRun(page *StoredCheckRunsPage, perPage int64, run StoredCheckRun) bool {
	// A page carries no sha of its own -- only its entries do.
	if !pageDescribesSHA(page, run.HeadSHA) {
		return false
	}
	// An added entry would spill onto the next page, which this document does not hold.
	if int64(len(page.CheckRuns))+1 > perPage {
		return false
	}
	at := sort.Search(len(page.CheckRuns), func(i int) bool { return page.CheckRuns[i].ID < run.ID })
	page.CheckRuns = slices.Insert(page.CheckRuns, at, run)
	page.TotalCount = int64(len(page.CheckRuns))
	return true
}

// InvalidateCommitCIForRefKind drops ref spelling's snapshots of kind. A
// check delivery uses it for the check-runs kind: a check run never appears
// in a commit's statuses. Flushing those rows too would re-fetch answers the
// delivery cannot have changed.
func (s *Store) InvalidateCommitCIForRefKind(ctx context.Context, owner, repo, ref, kind string) error {
	return s.qx(ctx).DeleteCommitCICacheForRefKind(ctx, dbgen.DeleteCommitCICacheForRefKindParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Ref: ref, Kind: kind,
	})
}

func (s *Store) deleteCommitCIRow(ctx context.Context, owner, repo string, row dbgen.CommitCiCache) error {
	return s.qx(ctx).DeleteCommitCICacheForRefKind(ctx, dbgen.DeleteCommitCICacheForRefKindParams{
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

// patchCombinedStatus rewrites a stored combined status from the delivery. The
// context's previous entry is REPLACED (the endpoint holds the latest per
// context) and the new lands at the end, where the array's oldest- order puts
// the newest status.
//
// Reports false -- the caller drops the row instead -- whenever the result is
// not provably what a fetch would return. A page other than the, a page that
// does not hold every context (total_count above its own length). A page with
// no room for a new context. This is or a status that is not the newest on
// the commit and so does not belong at the end.
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
// list is append-only history, newest-, so the new status is PREPENDED -- the
// context's older entries stay, exactly as upstream keeps them. A page that
// overflows drops its oldest entry, which is precisely where page begins.
// Every other page of this ref is dropped by the caller's fallback, since
// their contents shift.
//
// Reports false for a page other than the, an unreadable document, or a
// status older than the already at the head (its position is then unknown).
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
