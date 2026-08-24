# Why the published binary drops darwin/arm64

`ci.yml`'s `build` job pins `cosmo-platforms: linux/amd64,windows/amd64` —
narrower than `wow-look-at-my/go-toolchain@v1`'s own default
(`linux/amd64,darwin/arm64,windows/amd64`). This is a temporary mitigation,
not a design choice: any cosmo cross-build for `arm64` fails while compiling
this repo's `modernc.org/sqlite` dependency, for reasons entirely outside this
repo.

## The mechanism

`go-toolchain matrix` patches third-party modules with no native `GOOS=cosmo`
port (`internal/cosmocompat` in the go-toolchain repo). Its
`modernc.org/sqlite` gap table copies every file under `lib/` matching an
arch's build constraint — for `arm64` that includes `lib/hooks_linux_arm64.go`
— and force-tags the copy `cosmo`. It never excludes the arch-generic sibling
`lib/hooks.go`, tagged `!(linux && arm64)`. Under `GOOS=cosmo`, `GOOS` is never
literally `"linux"`, so that negative tag is always true: `hooks.go` compiles
alongside the new cosmo copy, and both declare `X__ccgo_sqlite3_log` and
`PatchIssue199`.

Verified 2026-08-23 with `modernc.org/sqlite@v1.57.0` — the exact version
go-toolchain's own gap table claims to be verified against — so this is not a
version-pin problem on this repo's side. The gap table is missing a
`tagEdit{path: "lib/hooks.go"}` (its own documented mechanism, in
`src/cosmocompat/gaps.go`) to append `&& !cosmo` once an arch-specific cosmo
copy exists.

## Restoring darwin/arm64

Once go-toolchain's `modernc.org/sqlite` gap table carries that `tagEdit`,
restore the default by deleting the `with: cosmo-platforms:` override in
`ci.yml`'s `build` job (and this file).
