# CI workflow (`.github/workflows/ci.yml`)

The reasoning behind each job's shape and each permission grant. The workflow itself keeps one-line pointers here. This is where the depth lives.

## Permissions block

Each grant guards a specific hard failure, not a general "be safe" default.

- `id-token: write` — OIDC for secret-server and buildhost autorelease.
- `contents: write` — go-toolchain submits a dependency-graph snapshot. GitHub rejects the submission with HTTP 403 under `contents: read`.
- `actions: read` + `checks: read` — the no-all-builds guard scans this run's jobs through the Actions API. It also scans the head commit's check runs through the Checks API. It then fails closed on a job named `all-builds`. See `wow-look-at-my/actions@no-all-builds-job#latest` and `wow-org-rulesets.md`.
- `artifact-metadata: write` + `deployments: write` — go-toolchain's release upload in `build`, and `buildhost-publish-site` in `preview`, both record the publish. Each records it as a GitHub Deployment and on the org's linked-artifacts page. That recording is part of publishing, not a best-effort extra. Both jobs fail without these grants.
- `pull-requests: write` — `buildhost-publish-site` hands the preview URL to the branch's PR as a comment. That hand-off is part of publishing and has no opt-out here. Without the grant, `preview` fails with HTTP 403 on every PR branch. Master runs never showed it, because there is no PR to comment on.
- `packages: write` — the `publish-ghcr` reusable workflow pushes the image to GHCR. A reusable workflow cannot exceed the caller's own grant, so the caller declares it too.

## `web-check`

The dashboard front-end has exactly ONE source: TypeScript in `internal/api/web/src/*.ts`. `npm run build` (tsc) emits `internal/api/web/assets/*.js` as a build output. That output is gitignored and never committed. Every job that needs it compiles it, and `build` compiles it before the Go build embeds it.

This job is the type-check gate, because tsc exits non-zero on any error. It also enforces the no-committed-JS rule. A generated file thus never creeps back into the tree as a second, staleable source of truth.

The test harnesses in `internal/api/testdata/` are TypeScript. `npm run check:harness` checks them against the dashboard source they drive. A changed export signature thus fails here, rather than at runtime inside a measurement. node runs the `.ts` files directly, so this step emits nothing.

## `build`

The dashboard JS is not committed, so this job compiles it from the TypeScript before the Go build. `internal/api/dashboard.go` names each `assets/*.js` file in its `//go:embed` line. A missing embed target is a compile error, not a runtime 404.

`go-toolchain@v1` cache-uploads `build/` as the `go-build` hand-off on every run. The key is once-per-run, not job-disambiguated. `publish-ghcr` and `image-smoke` both cache-download it. Do not add an explicit `cache-upload go-build` step here. A second same-run upload of that name collides on the key and fails.

## `image-smoke`

START the image and require it to serve. Nothing did that before. `build` tests the HOST binary, and `publish-ghcr` builds and pushes without ever running the result. The first execution of the entrypoint was thus production. An image whose binary the kernel cannot exec once shipped green this way. The whole fleet's GitHub traffic routes through this service, so every route 404'd. See CLAUDE.md's APE-staging bullet and `docker/imagecheck.test.ts`.

`path: build` must match what `publish-ghcr` passes. The hand-off holds the binaries themselves, not a `build/` directory. A restore into the workspace root thus leaves the Dockerfile's `COPY` with nothing to find.

The assertions live in `docker/imagecheck.test.ts`, not in this job. This job only restores the binary and runs the suite. An engineer thus reproduces a failure with `npm run test:image`, instead of pushing a commit to see it.

## `publish-ghcr`

Builds the Docker image and pushes `ghcr.io/wow-look-at-my/github-state-mirror:latest` on master. It restores the `go-build` hand-off that `build` cache-uploaded, which is `build/server`, the cosmo fat APE. It feeds that binary to the Dockerfile, then prunes old GHCR versions. The job is gated on `image-smoke`. An image nobody started is exactly what shipped the outage above.

## `preview`

Deploys a standalone, backend-free styling preview of the dashboard to buildhost, as a per-branch static site at `https://sites.pazer.build/github-state-mirror/branch/<branch>/`. The preview injects `demo-data.js`, so the page renders canned login, user and admin views without a real server. OIDC handles auth, and no static token is necessary.

Its "Assemble the demo bundle" step injects the preview-only `demo-data.js` script tag before `app.js`. The page thus renders canned data instead of calling the absent backend. The step also flattens the branch name to buildhost's single-path-segment convention. Any character outside `[A-Za-z0-9._-]` becomes `-`, because buildhost serves a site under one path segment.
