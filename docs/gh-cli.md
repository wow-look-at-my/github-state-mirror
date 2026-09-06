# Pointing the `gh` CLI at the mirror

`GH_HOST=github-state-mirror.pazer.io gh ...` sends every REST and GraphQL call
through the mirror, so `gh api`, `gh pr list`, `gh repo view` and the rest read
the cache instead of spending GitHub rate-limit budget.

## What gh does with a non-github.com host

gh classifies any host that is not `github.com` (or a `*.ghe.com` tenancy) as
GitHub Enterprise Server, and that changes three things:

1. **REST is prefixed with `/api/v3`.** `gh api user` requests
   `https://<host>/api/v3/user`.
2. **GraphQL moves to `/api/graphql`**, not `/graphql`.
3. **The token comes from `GH_ENTERPRISE_TOKEN`.** `GH_TOKEN` and
   `GITHUB_TOKEN` are read only for github.com, so a call with GH_TOKEN set and
   nothing else reaches the mirror with no `Authorization` header and gets 401.

`internal/api/ghecompat.go` answers the first two. It is a middleware mounted
ahead of the router, so it canonicalises `/api/v3/<path>` to `/<path>` and
`/api/graphql` to `/graphql` before anything else sees the request. The cached
routes, the reveal layer, the request log and the passthrough proxy therefore
keep working in api.github.com paths only, and the passthrough forwards the
canonical path upstream. `/api/v3` and `/api/v3/` map to `/`, which is the root
probe gh makes when it checks a host.

The rewrite grants nothing. A prefixed request meets `requireAuth` and the
reveal layer exactly as the plain spelling does.

`/api/v3` cannot collide with the dashboard's own `/api/*` routes: nothing under
`/api/` on the dashboard is named `v3` or `graphql`.

## Usage

```sh
export GH_HOST=github-state-mirror.pazer.io
export GH_ENTERPRISE_TOKEN="$YOUR_PAT"       # not GH_TOKEN
gh api user
gh api repos/OWNER/REPO
gh pr list -R OWNER/REPO
```

Pass `-R OWNER/REPO`. gh resolves a bare command's repository from the git
remote, and a github.com remote does not belong to the host you selected.

In a Claude Code web session the `gh` shim in `claude-code-web-config` does the
two awkward parts for you: it accepts `GH_HOST=https://github-state-mirror.pazer.io`
(stripping the scheme gh cannot parse) and copies the session's PAT into
`GH_ENTERPRISE_TOKEN`.

## Verified

Against a locally built server behind TLS, with `GH_HOST` naming it:

- `gh api user` and `gh api repos/PazerOP/dummy-repo-pazerop` — 200, and the
  second call answers `X-Gsm-Cache: hit`.
- `gh auth status` — logged in (it asks GraphQL for `viewer{login}`).
- `gh pr list -R wow-look-at-my/github-state-mirror` and `gh repo view --json`
  — both GraphQL, both answered.

The server log shows gh really does call `/api/v3/user`, `/api/v3/` and
`/api/graphql`.
