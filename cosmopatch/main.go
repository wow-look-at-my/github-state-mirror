//go:build cosmopatch_tool

// cosmopatch generates GOOS=cosmo build support for the three
// third-party modules the cosmo fat-APE release build needs but that
// carry no upstream cosmo port: modernc.org/libc, golang.org/x/sys,
// and modernc.org/sqlite. See README.md in this directory for why
// this exists and how the mechanism works.
//
// It downloads the pristine module versions pinned in go.mod (bypassing
// this repo's own `replace` directives, which point at ITS output),
// copies each module into an output tree under .cosmopatch-out/, adds a
// cosmo-tagged sibling of a chosen platform file for every gap the cosmo
// build hits, and appends "&& !cosmo" to a short list of upstream files
// whose existing negative build tag would otherwise also match cosmo and
// collide with the new sibling. Run it with:
//
//	go run ./cosmopatch/main.go ./cosmopatch/tables.go
//
// before `go-toolchain matrix` (ci.yml's release job does this
// automatically). It is idempotent and safe to re-run.
//
// Both files carry a fictitious "cosmopatch_tool" tag -- never set by
// any real build -- rather than the conventional "//go:build ignore",
// and are run as explicit files (build constraints don't apply when `go
// run` is given file paths rather than a package path) rather than `go
// run ./cosmopatch`. Two things go wrong with the plain "ignore"
// convention here: go-toolchain has a dedicated vet pass for
// "//go:build ignore" files that activates the `ignore` tag globally,
// which also flips on OTHER "//go:build ignore" generator scripts this
// generator's own output makes reachable (modernc.org/sqlite and
// modernc.org/libc each ship one) -- two same-directory files with
// different package names under one active tag set is a build error.
// And a real (untagged) package here is not right either: go-toolchain
// matrix builds a cosmo fat APE for every `package main` it can reach,
// which would then also try to fat-APE-build this dev-time tool.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Keep these in lockstep with the require versions in go.mod. A mismatch
// means this generator would patch the wrong pristine source; the build
// check below catches that instead of silently patching stale code.
const (
	libcModule    = "modernc.org/libc"
	libcVersion   = "v1.70.0"
	sysModule     = "golang.org/x/sys"
	sysVersion    = "v0.42.0"
	sqliteModule  = "modernc.org/sqlite"
	sqliteVersion = "v1.48.0"
)

// A sibling of the repo checkout, deliberately NOT nested inside it:
// go-toolchain's //go:generate scanner walks the repo tree it is run
// from, and these third-party modules ship generate directives (a
// ccgo/sqlite regenerator, mkwinsyscall, ...) this repo has no reason to
// ever run. Living outside the tree keeps the scanner from seeing them
// at all, matching go.mod's "../.cosmopatch-out" replace targets.
const outDir = "../.cosmopatch-out"

// copySpec adds one cosmo-tagged file, built by copying an existing
// platform file from the same module and forcing its build tag to
// "cosmo" (or, if extraCond is set, "cosmo && <extraCond>" -- needed
// for race0.go/race.go, whose own +build-race/-race pair is otherwise
// lost entirely: neither's OS-keyed condition is ever true under
// GOOS=cosmo, so a flat "cosmo" tag on both copies would make them
// collide instead of staying mutually exclusive). src and dst are
// module-relative paths.
type copySpec struct {
	src, dst, extraCond string
}

// tagEdit appends " && !cosmo" to the //go:build line of an existing
// module file, so the cosmo build doesn't also pick up a file meant for
// some other platform (see README.md, "why the exclusions").
type tagEdit struct {
	path string
}

func main() {
	repoRoot, err := os.Getwd()
	must(err)
	verifyVersionsMatchGoMod(repoRoot)

	libcSrc := downloadModule(libcModule, libcVersion)
	sysSrc := downloadModule(sysModule, sysVersion)
	sqliteSrc := downloadModule(sqliteModule, sqliteVersion)

	libcOut := filepath.Join(repoRoot, outDir, "libc")
	sysOut := filepath.Join(repoRoot, outDir, "sys")
	sqliteOut := filepath.Join(repoRoot, outDir, "sqlite")

	freshCopy(libcSrc, libcOut)
	freshCopy(sysSrc, sysOut)
	freshCopy(sqliteSrc, sqliteOut)

	for _, e := range libcTagEdits {
		appendCosmoExclusion(filepath.Join(libcOut, e.path))
	}
	for _, e := range sysTagEdits {
		appendCosmoExclusion(filepath.Join(sysOut, e.path))
	}

	for _, c := range libcCopies {
		addCosmoFile(libcOut, c.src, c.dst, c.extraCond)
	}
	for _, c := range sysCopies {
		addCosmoFile(sysOut, c.src, c.dst, c.extraCond)
	}
	for _, c := range sqliteCopies {
		addCosmoFile(sqliteOut, c.src, c.dst, c.extraCond)
	}

	// Overlay the hand-written and hand-patched files last, so they win
	// over anything a copySpec produced at the same path.
	copyTree(filepath.Join(repoRoot, "cosmopatch", "overlay", "x-sys-unix"), filepath.Join(sysOut, "unix"))
	patchPrlimit(filepath.Join(sysOut, "unix", "syscall2_cosmo.go"))
	copyTree(filepath.Join(repoRoot, "cosmopatch", "overlay", "libc-root"), libcOut)

	fmt.Println("cosmopatch: wrote", outDir)
}

func verifyVersionsMatchGoMod(repoRoot string) {
	data, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	must(err)
	text := string(data)
	check := func(module, version string) {
		if !strings.Contains(text, module+" "+version) {
			fmt.Fprintf(os.Stderr, "cosmopatch: go.mod no longer requires %s %s -- update the version constant in cosmopatch/main.go to match, then re-verify every cosmo file this generator adds still applies to the new version\n", module, version)
			os.Exit(1)
		}
	}
	check(libcModule, libcVersion)
	check(sysModule, sysVersion)
	check(sqliteModule, sqliteVersion)
}

// downloadModule fetches the exact pinned version into GOMODCACHE,
// independent of this repo's own go.mod replace directives (which
// point at this generator's own output), and returns its directory.
func downloadModule(module, version string) string {
	ref := module + "@" + version
	cmd := exec.Command("go", "mod", "download", "-json", ref)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cosmopatch: go mod download %s: %v\n%s\n", ref, err, out)
		os.Exit(1)
	}
	// The JSON output's "Dir" field is on its own line; avoid a JSON
	// dependency for one field by scanning for it directly.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, `"Dir":`) {
			v := strings.TrimPrefix(line, `"Dir":`)
			v = strings.TrimSpace(v)
			v = strings.Trim(v, `",`)
			return v
		}
	}
	fmt.Fprintf(os.Stderr, "cosmopatch: go mod download %s: no Dir in output:\n%s\n", ref, out)
	os.Exit(1)
	return ""
}

func freshCopy(src, dst string) {
	must(os.RemoveAll(dst))
	must(os.MkdirAll(dst, 0o755))
	copyTree(src, dst)
}

func copyTree(src, dst string) {
	entries, err := os.ReadDir(src)
	if os.IsNotExist(err) {
		return
	}
	must(err)
	must(os.MkdirAll(dst, 0o755))
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyTree(s, d)
			continue
		}
		data, err := os.ReadFile(s)
		must(err)
		must(os.WriteFile(d, data, 0o644))
	}
}

// addCosmoFile copies an existing platform file within the module to a
// new cosmo-tagged sibling. The copy's //go:build line -- wherever it
// falls in the leading comment block -- is replaced outright with
// "//go:build cosmo" (or "//go:build cosmo && <extraCond>" when
// extraCond is set), and any leftover double //go:build line from a
// file whose header spans more than one such line is removed.
func addCosmoFile(moduleOut, src, dst, extraCond string) {
	data, err := os.ReadFile(filepath.Join(moduleOut, src))
	must(err)
	lines := strings.Split(string(data), "\n")

	tag := "//go:build cosmo"
	if extraCond != "" {
		tag = "//go:build cosmo && " + extraCond
	}

	out := make([]string, 0, len(lines)+2)
	replaced := false
	for _, l := range lines {
		if strings.HasPrefix(l, "//go:build ") {
			if replaced {
				// A second //go:build line in the same file (rare,
				// seen in a couple of generated headers): drop it,
				// the first replacement already set the constraint.
				continue
			}
			out = append(out, tag, "")
			replaced = true
			continue
		}
		out = append(out, l)
	}
	if !replaced && strings.HasSuffix(dst, ".s") {
		// Assembly has no package clause to anchor on; insert right
		// after the header comment line instead (matches abi0_linux_amd64.s,
		// the only .s file this table adds).
		final := make([]string, 0, len(out)+3)
		final = append(final, out[0], "", tag)
		final = append(final, out[1:]...)
		out = final
		replaced = true
	}
	if !replaced {
		// No existing tag (a filename-only constrained file): add one
		// right before the package clause.
		final := make([]string, 0, len(out)+2)
		for _, l := range out {
			if strings.HasPrefix(l, "package ") && !replaced {
				final = append(final, tag, "")
				replaced = true
			}
			final = append(final, l)
		}
		out = final
	}
	if !replaced {
		fmt.Fprintf(os.Stderr, "cosmopatch: %s: found neither a //go:build line to replace, nor a package clause to insert one before\n", src)
		os.Exit(1)
	}

	dstPath := filepath.Join(moduleOut, dst)
	must(os.MkdirAll(filepath.Dir(dstPath), 0o755))
	must(os.WriteFile(dstPath, []byte(strings.Join(out, "\n")), 0o644))
}

// appendCosmoExclusion wraps a file's existing //go:build expression in
// parens and ANDs in "!cosmo". The parens are required, not cosmetic:
// "&&" binds tighter than "||" in a build-tag expression, so appending
// "&& !cosmo" onto an OR'd expression without them (e.g. "!linux ||
// !go1.24") would silently parse as "!linux || (!go1.24 && !cosmo)"
// instead of the intended "(!linux || !go1.24) && !cosmo" -- exercised
// by vgetrandom_unsupported.go, whose original tag is exactly this
// shape. See README.md, "why the exclusions", for what this prevents.
func appendCosmoExclusion(path string) {
	data, err := os.ReadFile(path)
	must(err)
	lines := strings.Split(string(data), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, "//go:build ") {
			expr := strings.TrimPrefix(l, "//go:build ")
			lines[i] = "//go:build (" + expr + ") && !cosmo"
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "cosmopatch: %s has no //go:build line to exclude cosmo from\n", path)
		os.Exit(1)
	}
	must(os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644))
}

// patchPrlimit replaces syscall2_cosmo.go's copy of Prlimit, which
// upstream pulls from the standard "syscall" package with
// //go:linkname syscall.prlimit. The cosmo fork's syscall.prlimit
// carries no matching //go:linkname push pragma authorizing that pull,
// so the link fails; call the raw syscall directly instead. See
// README.md, "the Prlimit patch", for the full explanation.
func patchPrlimit(path string) {
	data, err := os.ReadFile(path)
	must(err)
	text := string(data)

	const old = `//go:linkname syscall_prlimit syscall.prlimit
func syscall_prlimit(pid, resource int, newlimit, old *syscall.Rlimit) error

func Prlimit(pid, resource int, newlimit, old *Rlimit) error {
	// Just call the syscall version, because as of Go 1.21
	// it will affect starting a new process.
	return syscall_prlimit(pid, resource, (*syscall.Rlimit)(newlimit), (*syscall.Rlimit)(old))
}`

	const new = `// The upstream x/sys pulls this from the "syscall" package with
// //go:linkname, so a prlimit(RLIMIT_NOFILE) here also clears the
// package's cached original limit and so is picked up by a later
// os/exec.StartProcess. The cosmo fork's syscall.prlimit carries no
// matching //go:linkname push pragma, so that pull cannot resolve here;
// call the raw syscall directly instead. Nothing in this codebase
// combines unix.Prlimit with os/exec, so the missing side channel is
// inert in practice.
func Prlimit(pid, resource int, newlimit, old *Rlimit) error {
	_, _, errno := Syscall6(SYS_PRLIMIT64, uintptr(pid), uintptr(resource), uintptr(unsafe.Pointer(newlimit)), uintptr(unsafe.Pointer(old)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}`

	if !strings.Contains(text, old) {
		fmt.Fprintf(os.Stderr, "cosmopatch: %s no longer contains the expected Prlimit block -- golang.org/x/sys must have changed it upstream; update the patch in cosmopatch/main.go\n", path)
		os.Exit(1)
	}
	text = strings.Replace(text, old, new, 1)
	must(os.WriteFile(path, []byte(text), 0o644))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "cosmopatch:", err)
		os.Exit(1)
	}
}
