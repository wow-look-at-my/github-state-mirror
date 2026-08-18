# cosmopatch

`go-toolchain@v1`'s build step compiles a `GOOS=cosmo` fat APE
unconditionally, covering linux/amd64, darwin/arm64, and windows/amd64 in
one binary (see `wow-look-at-my/go-toolchain`'s `docs/CMD.md`, "GOOS=cosmo
splits"). This repo's `internal/database` package uses
`modernc.org/sqlite`, which pulls in `modernc.org/libc` and
`golang.org/x/sys` -- and none of the three ship a cosmo port. Building
against the pristine modules fails at compile or link time with missing
symbols, missing types, or (for one `golang.org/x/sys/unix` function) an
unresolvable linkname pull.

Rather than dropping the cosmo build or vendoring hundreds of thousands
of lines of generated third-party source into this repo, `cosmopatch`
generates the missing cosmo support at build time: it downloads the
pristine module versions pinned in `go.mod`, adds a `cosmo`-tagged
sibling of a chosen existing platform file for each gap, and writes the
result to `../.cosmopatch-out/` -- a SIBLING of this checkout, not
nested inside it (see the comment on `outDir` in `main.go` for why).
`go.mod`'s three `replace` directives point there.

Run it before any `go` command touches this module -- `go.mod`'s
replace targets don't exist until it has:

```sh
go run ./cosmopatch/main.go ./cosmopatch/tables.go
```

Both files carry a fictitious `cosmopatch_tool` build tag, and are
passed as explicit files rather than a package path (build constraints
don't apply when `go run` is given files directly) -- see the comment
atop `main.go` for why neither the conventional `//go:build ignore` nor
an ordinary untagged package works here. CI's `build` job runs this
immediately before the `go-toolchain@v1` step.

## How each gap is closed

Every missing symbol traced back to one of two shapes:

- **A whole file is missing.** Something the build needs -- a type, a
  syscall wrapper, a constant table -- exists for `linux/amd64` and
  `linux/arm64` (or, in several cases, for every `linux` architecture the
  module already supports) but was never given a `cosmo` variant.
  `tables.go`'s `libcCopies`, `sysCopies`, and `sqliteCopies` name an
  existing source file and the sibling to create; `addCosmoFile` copies
  it and forces its build tag to `cosmo`.
- **A generic fallback file also matches `cosmo` by accident.** A file
  meant as "the implementation for every platform except the ones with
  their own", written as a negation (`!(linux && amd64)`, `!linux`, and
  similar), is also true under `GOOS=cosmo` -- cosmo satisfies "not
  linux" and "not `linux && amd64`" simultaneously, since cosmo is
  neither. Once a real cosmo sibling exists for the same symbols, this
  produces a "redeclared" error. `tables.go`'s `libcTagEdits` and
  `sysTagEdits` name the file; `appendCosmoExclusion` appends
  `&& !cosmo` to its build tag.

None of this content is invented: every added file is copied verbatim
from a source the module already ships and builds successfully on a
real platform (checked when this generator was written -- see the
per-symbol trail in the commit that introduced it). The one exception is
noted below.

## Why explicit build tags, not filename matching alone

Go's implicit filename convention (`name_GOOS.go`, `name_GOOS_GOARCH.go`)
only recognizes `GOOS`/`GOARCH` values the *running* toolchain knows
about. The standard toolchain `go vet` and `go build` use for everything
except the cosmo step has never heard of `cosmo` as a `GOOS` -- so a file
named e.g. `foo_cosmo_amd64.go` is filtered by its `amd64` arch suffix
only, with no OS constraint at all, and compiles unconditionally on a
plain `linux/amd64` build too. Every file `addCosmoFile` writes carries
an explicit `//go:build cosmo` line for exactly this reason: the
standard toolchain honors an explicit tag it doesn't recognize (treating
an unset `cosmo` tag as false) even though it can't parse the filename
convention for an unrecognized OS. The gosmopolitan fork's toolchain,
which does know `cosmo`, applies both the filename arch suffix and the
explicit tag -- redundant there, but that redundancy is what keeps the
file inert everywhere else.

## The Prlimit patch

`golang.org/x/sys/unix`'s `Prlimit` pulls the real syscall from the
standard `syscall` package with `//go:linkname syscall_prlimit
syscall.prlimit`, deliberately reusing `syscall.prlimit` rather than
issuing the raw syscall itself: doing so also clears the `syscall`
package's cached original `RLIMIT_NOFILE` value, which
`os/exec.StartProcess` later reads to decide whether to restore a
child's file-descriptor limit. That pull requires the target,
`syscall.prlimit`, to carry a matching *push*-style `//go:linkname`
pragma authorizing outside packages to link against it -- present in the
standard `syscall_linux.go`, but not in the gosmopolitan fork's
`syscall_cosmo.go`, so the link fails.

`patchPrlimit` (in `main.go`) replaces this one function, after the
normal copy-and-retag step, with a version that issues the
`SYS_PRLIMIT64` syscall directly. This drops the `os/exec` side channel:
a process that calls `unix.Prlimit(unix.RLIMIT_NOFILE, ...)` and then
starts a child with `os/exec` under cosmo may hand that child a stale
limit. Nothing in this codebase does that -- `unix.Prlimit` is unused
here, `Prlimit` exists only because it's part of the package's exported
surface -- so the gap is inert in practice. If that changes, the exact
place to look is `patchPrlimit`.

`cosmopatch/overlay/x-sys-unix/` holds the one fully hand-written file
this needs, `syscall_cosmo_gc.go`: the low-level `Syscall`/`Syscall6`/
`RawSyscall`/`RawSyscall6`/`Gettimeofday` trampolines other platforms
implement in assembly (`syscall_unix_gc.go`), delegated instead to the
cosmo fork's own already-native `syscall` package implementations of the
same functions.

## Keeping this in sync with go.mod

`main.go` hardcodes the exact `modernc.org/libc`, `golang.org/x/sys`, and
`modernc.org/sqlite` versions this generator was built against, and
checks them against `go.mod`'s `require` block before doing anything
else. A version bump in `go.mod` without a matching update here fails
loudly (`verifyVersionsMatchGoMod`) rather than silently patching a
version this generator was never verified against. Bumping one of these
three modules means re-running the whole verification: rebuild with
`go-toolchain matrix` and confirm the cosmo fat APE still links.
