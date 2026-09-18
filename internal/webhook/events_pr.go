package webhook

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// pull_request payload parsing. The embedded pull_request object is a full

// PRPayload holds the full PR data and labels parsed from a webhook payload.
type PRPayload struct {
	PR     dbgen.PullRequest
	Labels []dbgen.PrLabel
}

// ParsePRPayload extracts a full PR and its labels from a pull_request
// webhook's raw JSON (the embedded pull_request is a full REST-shaped PR
// object, so webhook-maintained rows stay rest-complete for the cached
// /pulls routes).
func ParsePRPayload(raw json.RawMessage) (PRPayload, error) {
	var body struct {
		PullRequest *struct {
			Number         int     `json:"number"`
			NodeID         string  `json:"node_id"`
			Title          string  `json:"title"`
			Body           *string `json:"body"`
			HTMLURL        string  `json:"html_url"`
			Draft          bool    `json:"draft"`
			State          string  `json:"state"`
			CreatedAt      string  `json:"created_at"`
			UpdatedAt      string  `json:"updated_at"`
			Additions      *int    `json:"additions"`
			Deletions      *int    `json:"deletions"`
			Mergeable      *bool   `json:"mergeable"`
			MergeableState string  `json:"mergeable_state"`
			User           *struct {
				Login     string `json:"login"`
				Type      string `json:"type"`
				AvatarURL string `json:"avatar_url"`
				HTMLURL   string `json:"html_url"`
			} `json:"user"`
			Head struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo *struct {
					Name  string `json:"name"`
					Owner struct {
						Login string `json:"login"`
					} `json:"owner"`
				} `json:"repo"`
			} `json:"base"`
			AutoMerge *struct {
				MergeMethod string `json:"merge_method"`
			} `json:"auto_merge"`
			MergeCommitSHA *string `json:"merge_commit_sha"`
			Labels         []struct {
				Name  string `json:"name"`
				Color string `json:"color"`
			} `json:"labels"`
			RequestedReviewers []json.RawMessage `json:"requested_reviewers"`
			RequestedTeams     []json.RawMessage `json:"requested_teams"`
		} `json:"pull_request"`
	}

	if err := json.Unmarshal(raw, &body); err != nil {
		return PRPayload{}, fmt.Errorf("parse PR webhook payload: %w", err)
	}
	if body.PullRequest == nil {
		return PRPayload{}, fmt.Errorf("parse PR webhook payload: no pull_request field")
	}
	gpr := body.PullRequest

	// Derive owner/repo from base.repo (always present for PR webhooks).
	var owner, repo string
	if gpr.Base.Repo != nil {
		owner = gpr.Base.Repo.Owner.Login
		repo = gpr.Base.Repo.Name
	}

	// Map REST state to the UPPER format used by the GraphQL-origin cache.
	state := "OPEN"
	switch gpr.State {
	case "closed":
		state = "CLOSED"
	case "open":
		state = "OPEN"
	}

	// Normalise timestamps to RFC3339 (GitHub REST already sends them this way).
	createdAt := normaliseTime(gpr.CreatedAt)
	updatedAt := normaliseTime(gpr.UpdatedAt)

	pr := dbgen.PullRequest{
		Owner:       owner,
		Repo:        repo,
		Number:      int64(gpr.Number),
		Title:       gpr.Title,
		Url:         gpr.HTMLURL,
		IsDraft:     boolToInt(gpr.Draft),
		State:       state,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		HeadRefName: nullStr(gpr.Head.Ref),
		BaseRefName: nullStr(gpr.Base.Ref),
		HeadRefOid:  nullStr(gpr.Head.SHA),
		// REST-only fields (absent from the GraphQL org-repos selection set)
		NodeID:     nullStr(gpr.NodeID),
		BaseRefOid: nullStr(gpr.Base.SHA),
	}
	if gpr.Body != nil {
		pr.Body = sql.NullString{String: *gpr.Body, Valid: true}
	}
	if gpr.Head.Repo != nil {
		pr.HeadRepoFullName = nullStr(gpr.Head.Repo.FullName)
	}
	if gpr.AutoMerge != nil {
		pr.AutoMergeMethod = nullStr(gpr.AutoMerge.MergeMethod)
	}
	if gpr.MergeCommitSHA != nil {
		pr.MergeCommitSha = nullStr(*gpr.MergeCommitSHA)
	}

	if gpr.Additions != nil {
		pr.Additions = sql.NullInt64{Int64: int64(*gpr.Additions), Valid: true}
	}
	if gpr.Deletions != nil {
		pr.Deletions = sql.NullInt64{Int64: int64(*gpr.Deletions), Valid: true}
	}
	if gpr.Mergeable != nil {
		m := "UNKNOWN"
		if *gpr.Mergeable {
			m = "MERGEABLE"
		} else {
			m = "CONFLICTING"
		}
		pr.Mergeable = sql.NullString{String: m, Valid: true}
	}
	// mergeable_state is the only field that says a strict up-to-date rule is
	if gpr.MergeableState != "" && gpr.MergeableState != "unknown" {
		pr.MergeableState = sql.NullString{String: gpr.MergeableState, Valid: true}
	}
	if gpr.User != nil {
		pr.AuthorLogin = nullStr(gpr.User.Login)
		pr.AuthorAvatar = nullStr(gpr.User.AvatarURL)
		pr.AuthorUrl = nullStr(gpr.User.HTMLURL)
		pr.AuthorType = nullStr(gpr.User.Type)
	}

	reviewCount := len(gpr.RequestedReviewers) + len(gpr.RequestedTeams)
	pr.ReviewRequestCount = sql.NullInt64{Int64: int64(reviewCount), Valid: true}

	var labels []dbgen.PrLabel
	for _, l := range gpr.Labels {
		labels = append(labels, dbgen.PrLabel{
			Owner:    owner,
			Repo:     repo,
			PrNumber: int64(gpr.Number),
			Name:     l.Name,
			Color:    l.Color,
		})
	}

	return PRPayload{PR: pr, Labels: labels}, nil
}

// MergedPRBaseTip is what a merged pull_request delivery says about the BASE
// BRANCH, as opposed to about the PR: merging created a commit on that
// branch, and the payload names it.
type MergedPRBaseTip struct {
	BaseRef        string    // the base branch's name ("master")
	MergeCommitSHA string    // the commit merging put on it
	MergedAt       time.Time // when GitHub says that happened
}

// ParseMergedPRBaseTip reads a pull_request payload's statement about its base
// branch's tip, and reports false for any delivery that does not make.
//
// Only a MERGED PR does. `merge_commit_sha` on an open PR is the throwaway
// test-merge commit, which is on no branch at all -- reading that as a tip
// would write a commit nobody can reach. The merged flag plus merged_at is
// what separates the, and merged_at is also what orders the statement
// against what the mirror already holds (ghdata.ApplyMergedBaseTip).
func ParseMergedPRBaseTip(raw json.RawMessage) (MergedPRBaseTip, bool) {
	var body struct {
		PullRequest *struct {
			Merged         bool    `json:"merged"`
			MergedAt       *string `json:"merged_at"`
			MergeCommitSHA *string `json:"merge_commit_sha"`
			Base           struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.PullRequest == nil {
		return MergedPRBaseTip{}, false
	}
	pr := body.PullRequest
	if !pr.Merged || pr.MergedAt == nil || pr.MergeCommitSHA == nil || pr.Base.Ref == "" {
		return MergedPRBaseTip{}, false
	}
	mergedAt, err := time.Parse(time.RFC3339, *pr.MergedAt)
	if err != nil {
		return MergedPRBaseTip{}, false
	}
	return MergedPRBaseTip{BaseRef: pr.Base.Ref, MergeCommitSHA: *pr.MergeCommitSHA, MergedAt: mergedAt}, true
}
