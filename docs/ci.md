# CI workflow (`.github/workflows/ci.yml`)

The reasoning behind each job's shape and each permission grant. The workflow
itself keeps one-line pointers here; this is where the depth lives.

## Permissions block

Each grant guards a specific hard failure, not a general "be safe" default:

- `id-token: write` — OIDC for secret-server and buildhost autorelease.
- `contents: write` — go-toolchain submits a dependency-graph snapshot;
  GitHub rejects the submission with HTTP 403 under `contents: read`.
- `actions: read` + `checks: read` — the no-all-builds guard scans this run's
  jobs (Actions API) and the head commit's check runs (Checks API) to fail
  closed on a job literally named `all-builds` (see
  `wow-look-at-my/actions@no-all-builds-job#latest` and
  `wow-org-rulesets.md`).
- `artifact-metadata: write` + `deployments: write` — go-toolchain's release
  upload (`build`) and `buildhost-publish-site` (`preview`) both record the
  publish as a GitHub Deployment and on the org's linked-artifacts page;
  since `buildhost#193` that recording is part of publishing, not a
  best-effort extra, so both jobs fail without these.
- `packages: write` — the `publish-ghcr` reusable workflow pushes the image
  to GHCR; a reusable workflow cannot exceed the caller's own grant, so it
  has to be declared here too.

## `web-check`

The dashboard front-end has exactly ONE source: TypeScript in
`internal/api/web/src/*.ts`. `npm run build` (tsc) emits
`internal/api/web/assets/*.js` as a build output — gitignored, never
committed, compiled by every job that needs it (`build` compiles it before
the Go build embeds it). This job is the type-check gate (tsc exits non-zero
on any error) and it enforces the no-committed-JS rule, so a generated file
can never creep back into the tree as a second, staleable source of truth.

The test harnesses in `internal/api/testdata/` are TypeScript checked
against the dashboard source they drive (`npm run check:harness`), so a
changed export signature fails here rather than at runtime inside a
measurement — node runs the `.ts` directly, so this step emits nothing.

## `build`

The dashboard JS is not committed, so it is compiled from the TypeScript
before the Go build: `internal/api/dashboard.go` names each `assets/*.js`
file in its `//go:embed` line, and a missing embed target is a compile
error, not a runtime 404.

`go-toolchain@v1` cache-uploads `build/` as the `go-build` hand-off on every
run, keyed once-per-run (not job-disambiguated) — `publish-ghcr` and
`image-smoke` both just cache-download it. Do not add an explicit
`cache-upload go-build` step here: a second same-run upload of that name
collides on the key and fails.

## `image-smoke`

START the image and require it to serve. Nothing used to: `build` tests the
HOST binary, and `publish-ghcr` builds and pushes without ever running the
result, so the first execution of the entrypoint was production — an image
whose binary the kernel could not exec once shipped green this way, and the
whole fleet's GitHub traffic routes through this service, so every route
404'd. See CLAUDE.md's APE-staging bullet and `docker/imagecheck.test.ts`.

`path: build` must match what `publish-ghcr` passes: the hand-off holds the
binaries themselves, not a `build/` directory, so restoring into the
workspace root would leave the Dockerfile's `COPY` with nothing to find.

The assertions live in `docker/imagecheck.test.ts`, not in this job — this
job only restores the binary and runs the suite, so an engineer reproduces a
failure with `npm run test:image` instead of pushing a commit to see it.

## `publish-ghcr`

Builds the Docker image and pushes `ghcr.io/wow-look-at-my/github-state-mirror:latest`
on master: restores the `go-build` hand-off (`build/server_cosmo_fat`, the
cosmo fat APE) `build` cache-uploaded, feeds it to the Dockerfile, then
prunes old GHCR versions. Gated on `image-smoke` — an image nobody started
is exactly what shipped the outage above.

## `preview`

Deploys a standalone, backend-free styling preview of the dashboard to
buildhost as a per-branch static site
(`https://sites.pazer.build/github-state-mirror/branch/<branch>/`). The
preview injects `demo-data.js` so the page renders canned data (login/user/
admin views) without a real server. OIDC handles auth; no static token is
needed.

Its "Assemble the demo bundle" step injects the preview-only
`demo-data.js` script tag before `app.js` so the page renders canned data
instead of calling the absent backend, and flattens the branch name to
buildhost's single-path-segment convention (anything outside
`[A-Za-z0-9._-]` becomes `-`, since buildhost serves sites under one path
segment).
