package webhook

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PushPayload is the minimal info applied directly from a push webhook.
type PushPayload struct {
	Owner    string
	Repo     string
	Ref      string // e.g. "refs/heads/main"
	PushedAt string // RFC3339

	// Fields for absorbing the pushed commits into the git-commits cache.
	Before  string // sha of the ref before the push (all-zeros for a new ref)
	After   string // sha of the ref after the push
	Forced  bool
	Commits []PushCommit // pushed commits, payload order (oldest first)
}

// Branch returns the pushed branch name, or "" for a non-branch ref (tags).
func (p PushPayload) Branch() string {
	const prefix = "refs/heads/"
	if strings.HasPrefix(p.Ref, prefix) {
		return p.Ref[len(prefix):]
	}
	return ""
}

// PushCommit is one commit object from a push payload. The payload states the
// commit's id, tree id, message, timestamp, and author/committer identities --
// exactly the state GET /repos/{o}/{r}/git/commits/{sha} returns -- but NOT
// its parents; those are derived (see ChainedCommits).
type PushCommit struct {
	ID             string
	TreeID         string
	Message        string
	Timestamp      string // RFC3339 (normalised)
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
}

// ParsePushPayload extracts owner/repo, a best-effort pushed_at timestamp, and
// the pushed commits (for git-commit cache absorption).
func ParsePushPayload(raw json.RawMessage) (PushPayload, error) {
	var body struct {
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		HeadCommit *struct {
			Timestamp string `json:"timestamp"`
		} `json:"head_commit"`
		Ref     string `json:"ref"`
		Before  string `json:"before"`
		After   string `json:"after"`
		Forced  bool   `json:"forced"`
		Commits []struct {
			ID        string `json:"id"`
			TreeID    string `json:"tree_id"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
			Author    struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"committer"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return PushPayload{}, fmt.Errorf("parse push payload: %w", err)
	}
	if body.Repository == nil {
		return PushPayload{}, fmt.Errorf("parse push payload: no repository field")
	}
	p := PushPayload{
		Owner:  body.Repository.Owner.Login,
		Repo:   body.Repository.Name,
		Ref:    body.Ref,
		Before: body.Before,
		After:  body.After,
		Forced: body.Forced,
	}
	if body.HeadCommit != nil && body.HeadCommit.Timestamp != "" {
		p.PushedAt = normaliseTime(body.HeadCommit.Timestamp)
	} else {
		p.PushedAt = time.Now().UTC().Format(time.RFC3339)
	}
	for _, c := range body.Commits {
		p.Commits = append(p.Commits, PushCommit{
			ID:             c.ID,
			TreeID:         c.TreeID,
			Message:        c.Message,
			Timestamp:      normaliseTime(c.Timestamp),
			AuthorName:     c.Author.Name,
			AuthorEmail:    c.Author.Email,
			CommitterName:  c.Committer.Name,
			CommitterEmail: c.Committer.Email,
		})
	}
	if p.Owner == "" || p.Repo == "" {
		return PushPayload{}, fmt.Errorf("parse push payload: missing owner/repo")
	}
	return p, nil
}

// maxChainedPushCommits is the payload size at which parent derivation stops
// trusting the commits array: GitHub caps the array (larger pushes are
// truncated), and a truncated array breaks the before -> commits[0] chain.
const maxChainedPushCommits = 20

// ChainedCommits returns the pushed commits with a trustworthy linear parent
// chain -- commits[0]'s parent is `before`, each subsequent commit's parent is
// its predecessor -- or nil when the derivation cannot be trusted: a forced
// push (before is not the parent), a new ref (before is all zeros), a
// possibly-truncated array, or an array whose last id is not `after`
// (non-linear ordering). A pushed MERGE commit is the one case the payload
// cannot reveal (its extra parents are simply absent from the chain); the real
// consumer of cached parents -- pr-minder's test-merge inspection -- only
// reads parents on refs/pull/N/merge commits, which never arrive via push
// payloads (always fetch-sourced). So the chain is absorbed for the common
// linear push and dropped whenever any signal says otherwise.
func (p PushPayload) ChainedCommits() []PushCommit {
	n := len(p.Commits)
	if n == 0 || n >= maxChainedPushCommits || p.Forced {
		return nil
	}
	if !isRealSHA(p.Before) || p.Commits[n-1].ID != p.After {
		return nil
	}
	out := make([]PushCommit, n)
	copy(out, p.Commits)
	return out
}

// ParentForChained returns the derived parent sha for the i-th commit of a
// ChainedCommits slice: `before` for the first, the previous commit otherwise.
func (p PushPayload) ParentForChained(chain []PushCommit, i int) string {
	if i == 0 {
		return p.Before
	}
	return chain[i-1].ID
}

// isRealSHA reports whether s is a non-zero full-length hex object id (the
// all-zeros sha marks a created ref, which has no before-parent).
func isRealSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	real := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
		if c != '0' {
			real = true
		}
	}
	return real
}
