package ghclient

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// REST response types (only for JSON unmarshaling, not exported beyond this package)

type userResp struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	HTMLURL   string `json:"html_url"`
}

type orgResp struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	URL       string `json:"url"`
}

type compareResp struct {
	AheadBy  int `json:"ahead_by"`
	BehindBy int `json:"behind_by"`
}

type prFileResp struct {
	Filename  string `json:"filename"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

func (c *Client) GetAuthenticatedUser(ctx context.Context) (dbgen.User, error) {
	var resp userResp
	if err := c.doJSON(ctx, "GET", "/user", nil, &resp); err != nil {
		return dbgen.User{}, err
	}
	return dbgen.User{
		Login:     resp.Login,
		AvatarUrl: resp.AvatarURL,
		Url:       resp.HTMLURL,
	}, nil
}

func (c *Client) GetUserOrgs(ctx context.Context) ([]dbgen.Org, error) {
	var resp []orgResp
	if err := c.doJSON(ctx, "GET", "/user/orgs", nil, &resp); err != nil {
		return nil, err
	}
	orgs := make([]dbgen.Org, len(resp))
	for i, o := range resp {
		orgs[i] = dbgen.Org{
			Login:     o.Login,
			AvatarUrl: sql.NullString{String: o.AvatarURL, Valid: o.AvatarURL != ""},
			Url:       sql.NullString{String: o.URL, Valid: o.URL != ""},
		}
	}
	return orgs, nil
}

func (c *Client) CompareBranches(ctx context.Context, owner, repo, base, head string) (dbgen.BranchComparison, error) {
	path := fmt.Sprintf("/repos/%s/%s/compare/%s...%s", owner, repo, base, head)
	var resp compareResp
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return dbgen.BranchComparison{}, err
	}
	return dbgen.BranchComparison{
		Owner:    owner,
		Repo:     repo,
		BaseRef:  base,
		HeadRef:  head,
		AheadBy:  int64(resp.AheadBy),
		BehindBy: int64(resp.BehindBy),
	}, nil
}

func (c *Client) GetPRFiles(ctx context.Context, owner, repo string, number int64) ([]dbgen.PrFile, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files", owner, repo, number)
	var resp []prFileResp
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	files := make([]dbgen.PrFile, len(resp))
	for i, f := range resp {
		files[i] = dbgen.PrFile{
			Owner:     owner,
			Repo:      repo,
			PrNumber:  number,
			Path:      f.Filename,
			Additions: int64(f.Additions),
			Deletions: int64(f.Deletions),
		}
	}
	return files, nil
}
