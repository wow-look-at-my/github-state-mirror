package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

func TestAppPrivateKeyPEM_Inline(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n"
	c := Config{GitHubAppPrivateKey: pem}
	got, err := c.AppPrivateKeyPEM()
	require.NoError(t, err)
	assert.Equal(t, pem, string(got))
}

func TestAppPrivateKeyPEM_InlineEscapedNewlines(t *testing.T) {
	c := Config{GitHubAppPrivateKey: `-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n`}
	got, err := c.AppPrivateKeyPEM()
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n", string(got))
}

func TestAppPrivateKeyPEM_Path(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	content := "-----BEGIN RSA PRIVATE KEY-----\nfromfile\n-----END RSA PRIVATE KEY-----\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	// Path takes precedence over an inline value.
	c := Config{GitHubAppPrivateKeyPath: path, GitHubAppPrivateKey: "ignored"}
	got, err := c.AppPrivateKeyPEM()
	require.NoError(t, err)
	assert.Equal(t, content, string(got))
}

func TestAppPrivateKeyPEM_PathError(t *testing.T) {
	c := Config{GitHubAppPrivateKeyPath: "/no/such/file.pem"}
	_, err := c.AppPrivateKeyPEM()
	assert.Error(t, err)
}

func TestAppPrivateKeyPEM_Unset(t *testing.T) {
	c := Config{}
	got, err := c.AppPrivateKeyPEM()
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGitHubAppConfigured(t *testing.T) {
	assert.False(t, Config{}.GitHubAppConfigured())
	assert.True(t, Config{GitHubAppID: "42"}.GitHubAppConfigured())
}

// TestLoad_CacheMaxRowsDefault: an absent (or empty) CACHE_MAX_ROWS keeps the
// default ceiling.
func TestLoad_CacheMaxRowsDefault(t *testing.T) {
	t.Setenv("CACHE_MAX_ROWS", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, defaultCacheMaxRows, cfg.CacheMaxRows)
	assert.Equal(t, int64(1_000_000), cfg.CacheMaxRows)
}

// TestLoad_CacheMaxRowsValid: a valid override is applied verbatim.
func TestLoad_CacheMaxRowsValid(t *testing.T) {
	t.Setenv("CACHE_MAX_ROWS", "50000")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, int64(50000), cfg.CacheMaxRows)
}

// TestLoad_CacheMaxRowsInvalid: an unparseable or < 1 value must fail Load (a
// loud misconfiguration -- the server refuses to start), never fall back
// silently to a cap the operator didn't set.
func TestLoad_CacheMaxRowsInvalid(t *testing.T) {
	for _, v := range []string{"abc", "1.5", "10k", "0", "-5"} {
		t.Setenv("CACHE_MAX_ROWS", v)
		_, err := Load()
		assert.Error(t, err, "CACHE_MAX_ROWS=%q must fail Load", v)
	}
}

// TestCacheMaxRowsDefaultMatchesGhdata pins the config default to
// ghdata.CacheMaxRows' own initializer, so a consumer that never runs
// config.Load (tests, library use) sees the same ceiling the server defaults
// to and the two literals cannot drift. (No test in THIS binary mutates the
// ghdata var, so it is read at its initializer value.)
func TestCacheMaxRowsDefaultMatchesGhdata(t *testing.T) {
	assert.Equal(t, ghdata.CacheMaxRows, defaultCacheMaxRows)
}
