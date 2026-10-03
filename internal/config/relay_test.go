package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRelayURLs_UnsetMeansNone(t *testing.T) {
	for _, s := range []string{"", "   "} {
		got, err := parseRelayURLs(s)
		require.NoError(t, err)
		assert.Empty(t, got)
	}
}

func TestParseRelayURLs_Valid(t *testing.T) {
	got, err := parseRelayURLs(" https://a.example/webhook , http://b.internal:8081/hook ")
	require.NoError(t, err)
	assert.Equal(t, []string{"https://a.example/webhook", "http://b.internal:8081/hook"}, got)
}

func TestParseRelayURLs_MalformedEntryFailsNamingIt(t *testing.T) {
	for _, tc := range []struct {
		value string
		names string
	}{
		{"https://a.example/x,ftp://b.example/y", "ftp://b.example/y"},
		{"a.example/webhook", "a.example/webhook"},
		{"https://", "https://"},
		{"https://a.example/x,https://a.example/x", "https://a.example/x"},
		{"http://a.example/%zz", "http://a.example/%zz"},
		{"https://a.example/x,,https://b.example/y", "entry 2 is empty"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := parseRelayURLs(tc.value)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "WEBHOOK_RELAY_URLS")
			assert.Contains(t, err.Error(), tc.names)
		})
	}
}

func TestLoad_RelayURLs(t *testing.T) {
	t.Setenv("WEBHOOK_RELAY_URLS", "https://a.example/webhook")
	c, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"https://a.example/webhook"}, c.WebhookRelayURLs)

	t.Setenv("WEBHOOK_RELAY_URLS", "nope")
	_, err = Load()
	require.Error(t, err)
}
