package api

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The dashboard page is pushed its data. It asks the server for data only at
// the call sites this file declares, and it runs only the interval timers this
// file declares. A tab that reads its own endpoint to stay current is a poll
// under steady traffic. A new timer is where the next one starts.

var (
	// A comment is a whole line, or the tail of one. A "/*" inside a string literal is code.
	pageLineComment  = regexp.MustCompile(`(?m)(^|\s)//.*$`)
	pageBlockComment = regexp.MustCompile(`(?m)^\s*(/\*|\*).*$`)
	pageTimer        = regexp.MustCompile(`(?m)^.*\bsetInterval\(.*$`)
	pageCall         = regexp.MustCompile(`\b(?:api<[^>]*>|fetch|fetcher|fetchDecoded)\(\s*([^,)]*)`)
	pageLiteralPath  = regexp.MustCompile("^[\"`]([^\"`?$]*)")
)

// pageSource is one front-end source with its comments removed.
func pageSource(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile("web/src/" + name)
	require.NoError(t, err)
	return pageLineComment.ReplaceAllString(pageBlockComment.ReplaceAllString(string(src), ""), "$1")
}

// pageCallSites counts the request call sites of one source by what each asks for.
// A literal path loses its query. A parameter declaration is not a call.
func pageCallSites(src string) map[string]int {
	sites := map[string]int{}
	for _, m := range pageCall.FindAllStringSubmatch(src, -1) {
		arg := strings.TrimSpace(m[1])
		if strings.Contains(arg, ": ") {
			continue
		}
		if lit := pageLiteralPath.FindStringSubmatch(arg); lit != nil {
			arg = lit[1]
		}
		sites[arg]++
	}
	return sites
}

func TestDashboardPageAsksOnlyWhereDeclared(t *testing.T) {
	want := map[string]map[string]int{
		"app.ts": {
			// The helpers themselves: api()'s own fetch, Copy JSON's read, and the preview's canned Timeline.
			"path": 3,
			// The preview's one read of canned data. The real page opens the stream for the tab.
			"VIEW_PATHS[scope] as string": 1,
			// Set-up: who is signed in. The second site asks again when the stream keeps failing.
			"/api/me": 2,
			// Set-up: the request ring, one time. The stream sends each later event.
			"/api/requests": 1,
			// Acts of the user: sign out, and the modals.
			"/logout":          1,
			"/api/brief":       1,
			"/api/cache/data":  2,
			"/api/cache/check": 1,
		},
		"timeline.ts": {
			// fetchTimelineBytes, the preview's fetcher, and the first page of the chart.
			"path": 3,
			// An act of the user: a pan into older history.
			"/api/timeline": 1,
		},
		"request-log.ts": {},
		"rate-meter.ts":  {},
	}
	for name, sites := range want {
		assert.Equal(t, sites, pageCallSites(pageSource(t, name)),
			"%s asks the server somewhere this test does not declare. A tab's data belongs on the stream: add a feed in liveFeedFor. Declare a call here only for set-up or an act of the user.", name)
	}
}

func TestDashboardPageRunsOnlyDeclaredIntervalTimers(t *testing.T) {
	want := map[string][]string{
		// The preview's stand-in for the stream, and the watchdog that asks whether the stream is alive.
		"app.ts": {"streamWatch = setInterval(demoTick, 2000);", "streamWatch = setInterval(() => {"},
		// The countdown text. It reads no data.
		"rate-meter.ts":  {"this.timer = window.setInterval(() => this.tick(), 1000);"},
		"timeline.ts":    nil,
		"request-log.ts": nil,
	}
	for name, timers := range want {
		var got []string
		for _, line := range pageTimer.FindAllString(pageSource(t, name), -1) {
			if strings.Contains(line, "ReturnType<typeof setInterval>") {
				continue
			}
			got = append(got, strings.TrimSpace(line))
		}
		assert.Equal(t, timers, got, "%s runs an interval timer this test does not declare. A timer that reads data is a poll.", name)
	}
}
