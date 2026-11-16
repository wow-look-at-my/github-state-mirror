// Checks API reads through the mirror's own App, for a caller whose token GitHub gives no Checks scope.

package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// checksScopeRefusal is GitHub's answer to a fine-grained PAT on the Checks API. No PAT permission opens that API.
const checksScopeRefusal = "Resource not accessible by personal access token"

// fineGrainedPATPrefix starts every fine-grained personal access token.
const fineGrainedPATPrefix = "github_pat_"

const (
	appInstallationsTTL = 10 * time.Minute
	// appTokenTTL is under the hour an installation token lives.
	appTokenTTL = 50 * time.Minute
)

// appMinter is the part of ghclient.AppAuthenticator the reader uses.
type appMinter interface {
	Installations(ctx context.Context) ([]ghclient.Installation, error)
	InstallationToken(ctx context.Context, installID int64) (string, error)
}

// appReader holds the App's installation tokens by account. A nil reader reads nothing.
type appReader struct {
	app appMinter
	now func() time.Time

	mu         sync.Mutex
	installs   map[string]int64
	installsAt time.Time
	tokens     map[int64]appToken
}

type appToken struct {
	token  string
	minted time.Time
}

func newAppReader(app *ghclient.AppAuthenticator) *appReader {
	if app == nil {
		return nil
	}
	return &appReader{app: app, now: time.Now, installs: map[string]int64{}, tokens: map[int64]appToken{}}
}

// token returns an installation token for owner's account. ok is false when the App is not installed there.
func (a *appReader) token(ctx context.Context, owner string) (token string, installID int64, ok bool) {
	if a == nil {
		return "", 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.installsAt.IsZero() || now.Sub(a.installsAt) >= appInstallationsTTL {
		list, err := a.app.Installations(ctx)
		if err != nil {
			return "", 0, false
		}
		a.installs = make(map[string]int64, len(list))
		for _, inst := range list {
			a.installs[strings.ToLower(inst.Account.Login)] = inst.ID
		}
		a.installsAt = now
	}
	id, found := a.installs[strings.ToLower(owner)]
	if !found {
		return "", 0, false
	}
	if t, have := a.tokens[id]; have && now.Sub(t.minted) < appTokenTTL {
		return t.token, id, true
	}
	tok, err := a.app.InstallationToken(ctx, id)
	if err != nil {
		return "", 0, false
	}
	a.tokens[id] = appToken{token: tok, minted: now}
	return tok, id, true
}

// forget drops a token GitHub no longer accepts.
func (a *appReader) forget(installID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, installID)
}

// fetchChecks fetches a Checks API route. The caller must already have passed reveal for owner's repository: read
// access to a repository is what GitHub asks for a check run. A fine-grained PAT cannot state it. viaApp reports
// that the App answered, so the answer is no proof of what the caller's own token can read.
func (h *handlers) fetchChecks(r *http.Request, owner string) (resp *http.Response, body []byte, overflow, viaApp bool, err error) {
	fineGrained := strings.HasPrefix(bearerToken(r), fineGrainedPATPrefix)
	if fineGrained {
		if resp, body, overflow, ok := h.fetchAsApp(r, owner); ok {
			return resp, body, overflow, true, nil
		}
	}
	resp, body, overflow, err = h.fetchUpstream(r, nil)
	if err != nil || fineGrained || !isChecksScopeRefusal(resp, body) {
		return resp, body, overflow, false, err
	}
	if aresp, abody, aoverflow, ok := h.fetchAsApp(r, owner); ok {
		resp.Body.Close()
		return aresp, abody, aoverflow, true, nil
	}
	return resp, body, overflow, false, nil
}

func isChecksScopeRefusal(resp *http.Response, body []byte) bool {
	return resp.StatusCode == http.StatusForbidden && upstreamErrorMessage(body) == checksScopeRefusal
}

// fetchAsApp repeats r with the App's installation token for owner.
func (h *handlers) fetchAsApp(r *http.Request, owner string) (*http.Response, []byte, bool, bool) {
	token, installID, ok := h.appRead.token(r.Context(), owner)
	if !ok {
		return nil, nil, false, false
	}
	target := h.gh.BaseURL() + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	principal := syncpkg.AppInstallationActor(installID)
	ctx := actor.WithName(actor.WithActor(withUpstreamDisposition(r.Context(), dispUpstream), principal), owner)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, false, false
	}
	copyForwardHeaders(req.Header, r.Header)
	req.Header.Del("X-Mirror-Identity")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := h.upstream.Do(req)
	if err != nil {
		return nil, nil, false, false
	}
	h.meter.Observe(principal, owner, resp)
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxAbsorbBodyBytes+1))
	if err != nil || resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			h.appRead.forget(installID)
		}
		return nil, nil, false, false
	}
	return resp, buf, len(buf) > maxAbsorbBodyBytes, true
}
