package webhook

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// pull_request payload parsing. The embedded pull_request object is a full
// REST-shaped PR, so rows webhooks maintain stay rest-complete for the cached
// /pulls routes -- which is why this parses the whole thing rather than the
// few fields the dispatcher branches on.

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
			Number    int     `json:"number"`
			NodeID    string  `json:"node_id"`
			Title     string  `json:"title"`
			Body      *string `json:"body"`
			HTMLURL   string  `json:"html_url"`
			Draft     bool    `json:"draft"`
			State     string  `json:"state"`
			CreatedAt string  `json:"created_at"`
			UpdatedAt string  `json:"updated_at"`
			Additions *int    `json:"additions"`
			Deletions *int    `json:"deletions"`
			Mergeable *bool   `json:"mergeable"`
			User      *struct {
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
		// that the cached /pulls routes rebuild from. Webhook payloads carry
		// them all, so webhook-maintained rows stay rebuild-complete.
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
