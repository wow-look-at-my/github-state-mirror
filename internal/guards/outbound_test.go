package guards

import (
	"fmt"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// observedClients is the complete list of places this service can originate
// an outbound HTTP request, and what makes each one visible on the
// dashboard's Timeline.
//
// The count is part of the declaration on purpose. Keying by file alone would
// let a second client appear in an already-listed file and inherit a sentence
// written about a different one -- which is how "it's covered" stops being
// true without anybody noticing. A mismatch here is not a nuisance to bump:
// it is the guard asking where the new one shows up.
var observedClients = map[string]struct {
	count int
	why   string
}{
	"internal/httpobs/httpobs.go": {1,
		"the observing constructor itself -- this client IS the mechanism"},
	"internal/ghclient/client.go": {2,
		"New and NewWithBaseURL; SetExchangeObserver wraps the transport, so every " +
			"call any helper makes is charted (disposition internal)"},
	"internal/api/proxy.go": {1,
		"the passthrough ReverseProxy; httpobs.Transport on its Transport field charts " +
			"the mirror->GitHub leg, including the debounced batches (disposition upstream)"},
	"internal/api/oauth.go": {1,
		"the github.com login relay; relayGitHubLogin records each call itself " +
			"(disposition relay) -- one call site, pinned by TestTimeline_OAuthRelayRecorded"},
	"internal/notify/notifier.go": {1,
		"subscriber notifications; the notifier records every attempt on the '=> notify' " +
			"lane with its attempt number and terminal flag, which a transport cannot know"},
}

// TestEveryOutboundClientIsObserved walks the repository for anything that
// can send a request and fails on one that has not been declared above.
func TestEveryOutboundClientIsObserved(t *testing.T) {
	fset := token.NewFileSet()
	found := map[string][]Outbound{}
	files := 0

	require.NoError(t, filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skippedDirs.Contains(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		// Tests spin up their own clients against httptest servers; they
		// reach no real network and are not what an operator is watching.
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		hits, err := FindOutbound(fset, path, src)
		if err != nil {
			return err
		}
		if len(hits) > 0 {
			rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+"/"))
			found[rel] = append(found[rel], hits...)
		}
		return nil
	}))
	require.Greater(t, files, 50, "the walk must actually reach the repository's source")

	var problems []string
	for _, file := range sortedKeys(found) {
		hits := found[file]
		declared, ok := observedClients[file]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: %d outbound client(s), NOT DECLARED\n    %s",
				file, len(hits), joinOutbound(hits)))
			continue
		}
		if declared.count != len(hits) {
			problems = append(problems, fmt.Sprintf(
				"%s: declared %d outbound client(s), found %d\n    %s\n    declared: %s",
				file, declared.count, len(hits), joinOutbound(hits), declared.why))
		}
		for _, h := range hits {
			if h.Kind == KindPackageLevel {
				problems = append(problems, fmt.Sprintf(
					"%s: %s -- http.DefaultClient has no transport to observe; use httpobs.Client", file, h))
			}
		}
	}
	// A declaration for a file that no longer sends anything is a sentence
	// the next reader will believe. Retire it with the code.
	for _, file := range sortedKeys(observedClients) {
		if _, ok := found[file]; !ok {
			problems = append(problems, fmt.Sprintf("%s: declared as an outbound client, but none found -- delete the entry", file))
		}
	}

	assert.Empty(t, problems, "every outbound HTTP client must be declared in observedClients "+
		"with what makes it visible on the dashboard. Wrap it in internal/httpobs and add the entry, "+
		"or say why it is observed some other way.\n\n%s", strings.Join(problems, "\n"))

	if len(problems) == 0 {
		t.Logf("outbound-visibility check: %d Go files, %d declared outbound clients, no undeclared senders", files, len(observedClients))
	}
}

// The guard's teeth, against fixtures that are not compiled so the walk above
// cannot see them.
func TestFindOutboundFindsEachSenderShape(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    []OutboundKind
	}{
		{"outbound_senders.go.txt", []OutboundKind{KindClient, KindReverseProxy, KindPackageLevel, KindPackageLevel}},
		{"clean.go.txt", nil},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			require.NoError(t, err)
			found, err := FindOutbound(token.NewFileSet(), tc.fixture, src)
			require.NoError(t, err)
			require.Len(t, found, len(tc.want), "findings: %v", found)
			for i, want := range tc.want {
				assert.Equal(t, want, found[i].Kind)
			}
		})
	}
}

func joinOutbound(hits []Outbound) string {
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		parts = append(parts, h.String())
	}
	return strings.Join(parts, "\n    ")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
