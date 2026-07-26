package ghclient

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// AppAuthenticator signs in as a GitHub App. It mints short-lived RS256 JWTs
// from the app's private key and exchanges them for per-installation access
// tokens. This is the service's only credential: there is no static service
// token, and it is used solely for background refreshes (never to serve API
// requests). The JWT authenticates app-level endpoints (/app/*); installation
// access tokens authenticate data endpoints scoped to whatever a given
// installation can see.
type AppAuthenticator struct {
	appID  string
	key    *rsa.PrivateKey
	client *Client
}

// Installation is a GitHub App installation (the app installed on one account).
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "Organization" or "User"
	} `json:"account"`
}

// NewAppAuthenticator parses the app's PEM-encoded private key (PKCS#1 or
// PKCS#8) and returns an authenticator that uses client for its HTTP calls.
func NewAppAuthenticator(appID string, privateKeyPEM []byte, client *Client) (*AppAuthenticator, error) {
	if appID == "" {
		return nil, errors.New("github app id is empty")
	}
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse github app private key: %w", err)
	}
	return &AppAuthenticator{appID: appID, key: key, client: client}, nil
}

// installationsPerPage is GitHub's maximum page size for GET /app/installations.
const installationsPerPage = 100

// Installations lists every installation of the app, paging until a short page
// (a single 100-cap page silently truncated fleets past 100 installations).
func (a *AppAuthenticator) Installations(ctx context.Context) ([]Installation, error) {
	jwt, err := a.mintJWT(time.Now())
	if err != nil {
		return nil, err
	}
	ctx = WithToken(ctx, jwt)
	var all []Installation
	for page := 1; ; page++ {
		var out []Installation
		path := fmt.Sprintf("/app/installations?per_page=%d&page=%d", installationsPerPage, page)
		if err := a.client.doJSON(ctx, "GET", path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out...)
		if len(out) < installationsPerPage {
			return all, nil
		}
	}
}

// InstallationToken mints a short-lived (~1h) access token for one installation.
func (a *AppAuthenticator) InstallationToken(ctx context.Context, installID int64) (string, error) {
	jwt, err := a.mintJWT(time.Now())
	if err != nil {
		return "", err
	}
	ctx = WithToken(ctx, jwt)
	var out struct {
		Token string `json:"token"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installID)
	if err := a.client.doJSON(ctx, "POST", path, nil, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("github returned an empty installation token for installation %d", installID)
	}
	return out.Token, nil
}

// mintJWT builds and signs an RS256 JWT for app-level authentication. GitHub
// requires exp within 10 minutes of iat; iat is backdated 60s to tolerate clock
// skew between this host and GitHub.
func (a *AppAuthenticator) mintJWT(now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))

	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// parseRSAPrivateKey decodes a PEM-encoded RSA private key, accepting both
// PKCS#1 ("RSA PRIVATE KEY", GitHub's default download) and PKCS#8 ("PRIVATE
// KEY") encodings.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found in private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not a valid PKCS#1 or PKCS#8 private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want RSA", parsed)
	}
	return key, nil
}
