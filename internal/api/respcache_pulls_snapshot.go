// The closed and all PR listings, and the absent-PR verdict.

package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The closed and all PR listings, and the absent-PR verdict.

const (
	// pullsSnapshotTTL backstops a missed pull_request delivery.
	pullsSnapshotTTL = 10 * time.Minute

	// pullVerdictTTL backstops a missed pull_request opened delivery.
	pullVerdictTTL = 10 * time.Minute

	pullsSnapshotMaxPage = 10
)

// pullsSnapshotState reports whether the query asks for a state that truth
// does not hold.
func pullsSnapshotState(q url.Values) bool {
	st := q["state"]
	return len(st) == 1 && (st[0] == "closed" || st[0] == "all")
}

// parsePullsSnapshotShape returns the canonical query of a closed or all
// listing, or false for a shape the cache does not model.
func parsePullsSnapshotShape(q url.Values) (string, bool) {
	canon := url.Values{}
	perPage, page := pullsDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return "", false
		}
		v := vals[0]
		switch key {
		case "state":
			if v != "closed" && v != "all" {
				return "", false
			}
			canon.Set(key, v)
		case "per_page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return "", false
			}
			perPage = n
		case "page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > pullsSnapshotMaxPage {
				return "", false
			}
			page = n
		case "sort":
			switch v {
			case "created", "updated", "popularity", "long-running":
			default:
				return "", false
			}
			canon.Set(key, v)
		case "direction":
			if v != "asc" && v != "desc" {
				return "", false
			}
			canon.Set(key, v)
		case "head":
			owner, branch, ok := strings.Cut(v, ":")
			if !ok || owner == "" || branch == "" || strings.ContainsFunc(v, isControlRune) {
				return "", false
			}
			canon.Set(key, v)
		case "base":
			if v == "" || strings.ContainsFunc(v, isControlRune) {
				return "", false
			}
			canon.Set(key, v)
		default:
			return "", false
		}
	}
	canon.Set("per_page", strconv.Itoa(perPage))
	canon.Set("page", strconv.Itoa(page))
	return canon.Encode(), true
}

// pullSnapshotItemJSON is the open list's item plus both fields that
// tell a closed PR from a merged one.
type pullSnapshotItemJSON struct {
	pullListItemJSON
	ClosedAt *string `json:"closed_at"`
	MergedAt *string `json:"merged_at"`
}

// cachedPullsSnapshot serves a closed or all listing page from its stored
// snapshot, behind the same reveal gate as the open list.
func (h *handlers) cachedPullsSnapshot(w http.ResponseWriter, r *http.Request, owner, repo string) {
	shape, ok := parsePullsSnapshotShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	owner, repo = ghdata.NormalizeRepoKey(owner), ghdata.NormalizeRepoKey(repo)
	h.serveRepoAnswer(w, r, repoAnswerRoute{
		key:      ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindPullsSnapshot, Shape: shape},
		denyKind: denyKindRepoPulls,
		resource: owner + "/" + repo + "/pulls",
		absorb: func(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
			return absorbPullsSnapshot(owner, repo, resp.StatusCode, body, now)
		},
	})
}

// Any other status, or an entry the render cannot hold, relays.
func absorbPullsSnapshot(owner, repo string, status int, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
	if status != http.StatusOK {
		return ghdata.RepoAnswer{}, false
	}
	var raw []restPRJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return ghdata.RepoAnswer{}, false
	}
	items := make([]pullSnapshotItemJSON, 0, len(raw))
	for _, p := range raw {
		pr, labels, ok := absorbRestPR(owner, repo, p)
		if !ok {
			return ghdata.RepoAnswer{}, false
		}
		items = append(items, pullSnapshotItemJSON{
			pullListItemJSON: renderPullListItem(pr, labels),
			ClosedAt:         normalizedRESTTimePtr(p.ClosedAt),
			MergedAt:         normalizedRESTTimePtr(p.MergedAt),
		})
	}
	doc, err := marshalTrimmed(items)
	if err != nil {
		return ghdata.RepoAnswer{}, false
	}
	return ghdata.RepoAnswer{Status: http.StatusOK, Doc: string(doc), ExpiresAt: now.Add(pullsSnapshotTTL)}, true
}

func normalizedRESTTimePtr(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	v := normalizeRESTTime(*s)
	return &v
}

func pullVerdictKey(owner, repo, numStr string) ghdata.RepoAnswerKey {
	return ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindPullVerdict, RefID: numStr}
}

// servePullVerdict answers from a stored absent-PR verdict.
func (h *handlers) servePullVerdict(w http.ResponseWriter, r *http.Request, owner, repo, numStr string) bool {
	a, ok, err := h.store.GetRepoAnswer(r.Context(), pullVerdictKey(owner, repo, numStr), time.Now())
	if err != nil {
		slog.Warn("pull verdict cache read failed", "owner", owner, "repo", repo, "number", numStr, "error", err)
		return false
	}
	if !ok {
		return false
	}
	h.writeRepoAnswer(w, r, a, true)
	return true
}

// A pull_request delivery for that number drops it.
func (h *handlers) storePullVerdict(w http.ResponseWriter, r *http.Request, owner, repo, numStr string, resp *http.Response, body []byte, overflow bool) bool {
	if overflow || resp.StatusCode != http.StatusNotFound {
		return false
	}
	now := time.Now()
	a, ok := verdictAnswer(resp.StatusCode, body, now.Add(pullVerdictTTL))
	if !ok {
		return false
	}
	a.Subject = numStr
	if err := h.store.PutRepoAnswer(r.Context(), pullVerdictKey(owner, repo, numStr), a, now); err != nil {
		slog.Warn("pull verdict cache write failed", "owner", owner, "repo", repo, "number", numStr, "error", err)
	}
	h.writeRepoAnswer(w, r, a, false)
	return true
}
