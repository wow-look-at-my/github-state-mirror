module github.com/wow-look-at-my/github-state-mirror

go 1.25.0

require (
	github.com/go-chi/chi/v5 v5.2.5
	github.com/stretchr/testify v1.11.1
	github.com/wow-look-at-my/js-snippets/timelinewire v0.0.0-20260810095912-05d7e2f99130 // go-toolchain:auto-branch
	modernc.org/sqlite v1.48.0
)

require github.com/wow-look-at-my/go-containers v0.0.0-20260818100925-5e01414a6ac3 // go-toolchain:auto-branch

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.42.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// These three replaces point at output `go run ./cosmopatch/main.go
// ./cosmopatch/tables.go` generates as a SIBLING of this checkout (not
// nested inside it -- see the comment on cosmopatch/main.go's outDir) --
// see cosmopatch/README.md. Run it before building; without it, these
// paths don't exist and every build fails (including non-cosmo ones,
// since a replace with no target is a hard error regardless of GOOS).
replace modernc.org/libc => ../.cosmopatch-out/libc

replace golang.org/x/sys => ../.cosmopatch-out/sys

replace modernc.org/sqlite => ../.cosmopatch-out/sqlite
