package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
