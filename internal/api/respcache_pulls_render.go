package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// ---- absorbing REST PR bodies ----

// restPRJSON is the upstream shape both PR routes parse: the "simple PR" of a
// list item, plus the merge-status fields only the single-PR response carries.
type restPRJSON struct {
	Number         int64   `json:"number"`
	NodeID         string  `json:"node_id"`
	State          string  `json:"state"`
	Title          string  `json:"title"`
	Body           *string `json:"body"`
	Draft          bool    `json:"draft"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	Merged         *bool   `json:"merged"`    // single-PR responses only
	Mergeable      *bool   `json:"mergeable"` // single-PR responses only
	Additions      *int64  `json:"additions"` // single-PR responses only
	Deletions      *int64  `json:"deletions"` // single-PR responses only
	HTMLURL        string  `json:"html_url"`
	User           *struct {
		Login     string `json:"login"`
		Type      string `json:"type"`
		AvatarURL string `json:"avatar_url"`
		HTMLURL   string `json:"html_url"`
	} `json:"user"`
	Labels []struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"labels"`
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
	RequestedReviewers []json.RawMessage `json:"requested_reviewers"`
	RequestedTeams     []json.RawMessage `json:"requested_teams"`
}

// absorbPullsListBody parses a /pulls 200 array into storable rows + labels.
// Reports false -- serve verbatim, store nothing -- for any other status or
// any item the model cannot hold.
func absorbPullsListBody(owner, repo string, status int, body []byte) ([]dbgen.PullRequest, map[int64][]dbgen.PrLabel, bool) {
	if status != http.StatusOK {
		return nil, nil, false
	}
	var raw []restPRJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, false
	}
	rows := make([]dbgen.PullRequest, 0, len(raw))
	labelsByPR := make(map[int64][]dbgen.PrLabel, len(raw))
	for _, item := range raw {
		pr, labels, ok := absorbRestPR(owner, repo, item)
		if !ok {
			return nil, nil, false
		}
		rows = append(rows, pr)
		labelsByPR[pr.Number] = labels
	}
	return rows, labelsByPR, true
}

// absorbRestPR converts one REST PR object into a pull_requests row (+ label
// rows), reporting false when required fields are missing. owner/repo come
// from the response's own base.repo (canonical casing, so rows collide with
// webhook/GraphQL-written ones); the URL values are only the fallback.
func absorbRestPR(urlOwner, urlRepo string, p restPRJSON) (dbgen.PullRequest, []dbgen.PrLabel, bool) {
	if p.Number <= 0 || p.NodeID == "" || p.User == nil || p.User.Login == "" ||
		p.Head.Ref == "" || p.Head.SHA == "" || p.Base.Ref == "" || p.Base.SHA == "" ||
		p.CreatedAt == "" || p.UpdatedAt == "" {
		return dbgen.PullRequest{}, nil, false
	}
	var state string
	switch p.State {
	case "open":
		state = "OPEN"
	case "closed":
		state = "CLOSED"
	default:
		return dbgen.PullRequest{}, nil, false
	}
	owner, repo := urlOwner, urlRepo
	if p.Base.Repo != nil && p.Base.Repo.Owner.Login != "" && p.Base.Repo.Name != "" {
		owner, repo = p.Base.Repo.Owner.Login, p.Base.Repo.Name
	}
	pr := dbgen.PullRequest{
		Owner:        owner,
		Repo:         repo,
		Number:       p.Number,
		Title:        p.Title,
		Url:          p.HTMLURL,
		IsDraft:      boolToInt64(p.Draft),
		State:        state,
		CreatedAt:    normalizeRESTTime(p.CreatedAt),
		UpdatedAt:    normalizeRESTTime(p.UpdatedAt),
		NodeID:       sql.NullString{String: p.NodeID, Valid: true},
		AuthorLogin:  sql.NullString{String: p.User.Login, Valid: true},
		AuthorType:   nullableStr(p.User.Type),
		AuthorAvatar: nullableStr(p.User.AvatarURL),
		AuthorUrl:    nullableStr(p.User.HTMLURL),
		HeadRefName:  sql.NullString{String: p.Head.Ref, Valid: true},
		HeadRefOid:   sql.NullString{String: p.Head.SHA, Valid: true},
		BaseRefName:  sql.NullString{String: p.Base.Ref, Valid: true},
		BaseRefOid:   sql.NullString{String: p.Base.SHA, Valid: true},
		ReviewRequestCount: sql.NullInt64{
			Int64: int64(len(p.RequestedReviewers) + len(p.RequestedTeams)), Valid: true,
		},
	}
	if p.Body != nil {
		pr.Body = sql.NullString{String: *p.Body, Valid: true}
	}
	if p.Head.Repo != nil {
		pr.HeadRepoFullName = nullableStr(p.Head.Repo.FullName)
	}
	if p.AutoMerge != nil {
		pr.AutoMergeMethod = nullableStr(p.AutoMerge.MergeMethod)
	}
	if p.MergeCommitSHA != nil {
		pr.MergeCommitSha = nullableStr(*p.MergeCommitSHA)
	}
	if p.Mergeable != nil {
		m := "CONFLICTING"
		if *p.Mergeable {
			m = "MERGEABLE"
		}
		pr.Mergeable = sql.NullString{String: m, Valid: true}
	}
	if p.Additions != nil {
		pr.Additions = sql.NullInt64{Int64: *p.Additions, Valid: true}
	}
	if p.Deletions != nil {
		pr.Deletions = sql.NullInt64{Int64: *p.Deletions, Valid: true}
	}
	labels := make([]dbgen.PrLabel, 0, len(p.Labels))
	for _, l := range p.Labels {
		labels = append(labels, dbgen.PrLabel{
			Owner: owner, Repo: repo, PrNumber: p.Number, Name: l.Name, Color: l.Color,
		})
	}
	return pr, labels, true
}

// ---- rebuilding trimmed PR bodies ----

// The rebuilt shapes: GitHub's list/single PR fields that carry STATE, minus
// every URL field and the untracked clutter (milestone, assignees, locked,
// author_association, ...). A superset of every field pr-minder and the
// pr-minder-reconcile hook read off mirror-served PR objects: number, state,
// draft, title, body, node_id, user.login/.type, labels[].name/.color,
// head.ref/.sha/.repo.full_name, base.ref/.sha, auto_merge, merge_commit_sha,
// created_at/updated_at, and (single) mergeable/merged.
type pullUserJSON struct {
	Login string `json:"login"`
	Type  string `json:"type,omitempty"`
}

type pullLabelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type pullRepoRefJSON struct {
	FullName string `json:"full_name"`
}

type pullHeadJSON struct {
	Ref  string           `json:"ref"`
	SHA  string           `json:"sha"`
	Repo *pullRepoRefJSON `json:"repo"` // null when the head repo is gone
}

type pullBaseJSON struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type pullAutoMergeJSON struct {
	MergeMethod string `json:"merge_method"`
}

type pullListItemJSON struct {
	NodeID         string             `json:"node_id"`
	Number         int64              `json:"number"`
	State          string             `json:"state"`
	Title          string             `json:"title"`
	User           pullUserJSON       `json:"user"`
	Body           *string            `json:"body"`
	Labels         []pullLabelJSON    `json:"labels"`
	CreatedAt      string             `json:"created_at"`
	UpdatedAt      string             `json:"updated_at"`
	MergeCommitSHA *string            `json:"merge_commit_sha"`
	Draft          bool               `json:"draft"`
	AutoMerge      *pullAutoMergeJSON `json:"auto_merge"`
	Head           pullHeadJSON       `json:"head"`
	Base           pullBaseJSON       `json:"base"`
}

type pullSingleJSON struct {
	pullListItemJSON
	Merged    bool  `json:"merged"`
	Mergeable *bool `json:"mergeable"`
}

func renderPullListItem(pr dbgen.PullRequest, labels []dbgen.PrLabel) pullListItemJSON {
	item := pullListItemJSON{
		NodeID:    pr.NodeID.String,
		Number:    pr.Number,
		State:     strings.ToLower(pr.State),
		Title:     pr.Title,
		User:      pullUserJSON{Login: pr.AuthorLogin.String, Type: pr.AuthorType.String},
		Labels:    make([]pullLabelJSON, 0, len(labels)),
		CreatedAt: pr.CreatedAt,
		UpdatedAt: pr.UpdatedAt,
		Draft:     pr.IsDraft != 0,
		Head:      pullHeadJSON{Ref: pr.HeadRefName.String, SHA: pr.HeadRefOid.String},
		Base:      pullBaseJSON{Ref: pr.BaseRefName.String, SHA: pr.BaseRefOid.String},
	}
	if pr.Body.Valid {
		item.Body = &pr.Body.String
	}
	if pr.MergeCommitSha.Valid && pr.MergeCommitSha.String != "" {
		item.MergeCommitSHA = &pr.MergeCommitSha.String
	}
	if pr.AutoMergeMethod.Valid && pr.AutoMergeMethod.String != "" {
		item.AutoMerge = &pullAutoMergeJSON{MergeMethod: pr.AutoMergeMethod.String}
	}
	if pr.HeadRepoFullName.Valid && pr.HeadRepoFullName.String != "" {
		item.Head.Repo = &pullRepoRefJSON{FullName: pr.HeadRepoFullName.String}
	}
	for _, l := range labels {
		item.Labels = append(item.Labels, pullLabelJSON{Name: l.Name, Color: l.Color})
	}
	return item
}

func renderSinglePull(pr dbgen.PullRequest, labels []dbgen.PrLabel) pullSingleJSON {
	out := pullSingleJSON{pullListItemJSON: renderPullListItem(pr, labels)}
	// Only OPEN PRs are ever rebuilt (hit gate + absorb gate), so merged is
	// false by definition.
	switch pr.Mergeable.String {
	case "MERGEABLE":
		v := true
		out.Mergeable = &v
	case "CONFLICTING":
		v := false
		out.Mergeable = &v
	}
	return out
}

// renderClosedPull renders the trimmed document a CLOSED/merged answer is
// stored and served as -- rendered once at absorb time, so hit and miss are
// byte-identical. renderPullListItem lowercases the stored state, so the doc
// carries "closed" exactly as GitHub does; merged is GitHub's own answer
// (unlike the open rebuild's by-definition false), and mergeable maps like
// renderSinglePull's (typically null for a closed PR).
func renderClosedPull(pr dbgen.PullRequest, labels []dbgen.PrLabel, merged bool) pullSingleJSON {
	out := pullSingleJSON{pullListItemJSON: renderPullListItem(pr, labels), Merged: merged}
	switch pr.Mergeable.String {
	case "MERGEABLE":
		v := true
		out.Mergeable = &v
	case "CONFLICTING":
		v := false
		out.Mergeable = &v
	}
	return out
}
