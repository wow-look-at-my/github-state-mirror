package guards

import (
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/go-containers/set"
)

// repoRoot is this package's directory, two levels down from the module root.
const repoRoot = "../.."

// skippedDirs are trees whose contents this repo does not author.
var skippedDirs = set.Of(
	".git",
	"node_modules",
	"dbgen",    // sqlc codegen
	"testdata", // fixtures, incl. this guard's own deliberately-bad ones
)

// TestNoJSONSplices walks every Go file in the repository and fails on JSON
// text built from anything but a marshaller. See jsonsplice.go for why no
// hand-placement of a value -- +, %s, %d, or %q -- is safe.
func TestNoJSONSplices(t *testing.T) {
	fset := token.NewFileSet()
	var all []Finding
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
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found, err := CheckFile(fset, path, src)
		if err != nil {
			return err
		}
		files++
		all = append(all, found...)
		return nil
	}))
	require.Greater(t, files, 50, "the walk must actually reach the repository's source")

	if len(all) > 0 {
		lines := make([]string, 0, len(all))
		for _, f := range all {
			lines = append(lines, f.String())
		}
		assert.Fail(t, "JSON built by hand rather than marshalled",
			"JSON text is produced by marshalling, full stop. Concatenation escapes nothing; %%s is raw; %%d "+
				"assumes a number; even %%q is GO quoting, not JSON. Build the document as a Go value and hand "+
				"it to encoding/json.\n\n%s", strings.Join(lines, "\n"))
		return
	}
	t.Logf("json-marshalling check: %d Go files, every JSON document either constant or marshalled", files)
}

// The guard's teeth, against fixtures that are not compiled (testdata/*.go.txt)
// so the walk above cannot see them. Each names the shape it pins.
func TestCheckFileFindsEachSpliceShape(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    []string // substrings, one per expected finding
	}{
		{"concat.go.txt", []string{"concatenation", "concatenation"}},
		{"formatverb.go.txt", []string{"format verb", "format verb", "format verb"}},
		{"clean.go.txt", nil},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			require.NoError(t, err)
			found, err := CheckFile(token.NewFileSet(), tc.fixture, src)
			require.NoError(t, err)
			require.Len(t, found, len(tc.want), "findings: %v", found)
			for i, want := range tc.want {
				assert.Contains(t, found[i].String(), want)
			}
		})
	}
}
