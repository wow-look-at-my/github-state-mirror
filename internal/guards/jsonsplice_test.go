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
)

// repoRoot is this package's directory, two levels down from the module root.
const repoRoot = "../.."

// skippedDirs are trees whose contents this repo does not author.
var skippedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dbgen":        true, // sqlc codegen
	"testdata":     true, // fixtures, incl. this guard's own deliberately-bad ones
}

// TestNoJSONSplices walks every Go file in the repository and fails on a value
// placed inside a JSON string literal's own quotes. See jsonsplice.go for what
// counts and why %q does not.
func TestNoJSONSplices(t *testing.T) {
	fset := token.NewFileSet()
	var all []Finding
	files := 0
	require.NoError(t, filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skippedDirs[d.Name()] {
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
		assert.Fail(t, "value spliced into JSON string quotes",
			"A value between a JSON string's own quotes is escaped by nothing -- the document is valid only "+
				"while the value happens to contain no quote, backslash or control character. Use %%q (which "+
				"supplies and escapes its own quotes), give the value its own non-string slot, or marshal the "+
				"document from a Go value.\n\n%s", strings.Join(lines, "\n"))
		return
	}
	t.Logf("json-splice check: %d Go files, no value inside JSON string quotes", files)
}

// The guard's teeth, against fixtures that are not compiled (testdata/*.go.txt)
// so the walk above cannot see them. Each names the shape it pins.
func TestCheckFileFindsEachSpliceShape(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    []string // substrings, one per expected finding
	}{
		{"concat.go.txt", []string{"concatenation"}},
		{"formatverb.go.txt", []string{"format verb"}},
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
