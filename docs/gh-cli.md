# Pointing the `gh` CLI at the mirror

`GH_HOST=github-state-mirror.pazer.io gh ...` sends every REST and GraphQL call through the mirror. `gh api`, `gh pr list` and `gh repo view` then read the cache instead of GitHub.

## What gh does with a host that is not github.com

gh classifies such a host as GitHub Enterprise Server. That changes three things.

1. REST gains an `/api/v3` prefix. `gh api user` requests `https://<host>/api/v3/user`.
2. GraphQL moves to `/api/graphql`, not `/graphql`.
3. The token comes from `GH_ENTERPRISE_TOKEN`. gh reads `GH_TOKEN` and `GITHUB_TOKEN` for github.com only. A call with only `GH_TOKEN` set thus arrives with no `Authorization` header, and the mirror answers 401.

`internal/api/ghecompat.go` answers the first two points. It is a middleware mounted ahead of the router. It canonicalises `/api/v3/<path>` to `/<path>`, and `/api/graphql` to `/graphql`, before the router sees the request.

The cached routes, the reveal layer, the request log and the passthrough proxy thus keep working in api.github.com paths. The proxy forwards the canonical path upstream. `/api/v3` and `/api/v3/` map to `/`, which is the root probe gh makes to check a host.

The rewrite grants nothing. A prefixed request meets `requireAuth` and the reveal layer exactly as the plain spelling does.

`/api/v3` cannot collide with the dashboard's own `/api/*` routes. No dashboard route under `/api/` is named `v3` or `graphql`.

## Usage

```sh
export GH_HOST=github-state-mirror.pazer.io
export GH_ENTERPRISE_TOKEN="$YOUR_PAT"       # not GH_TOKEN
gh api user
gh api repos/OWNER/REPO
gh pr list -R OWNER/REPO
```

Pass `-R OWNER/REPO`. gh resolves a bare command's repository from the git remote, and a github.com remote does not belong to the host you selected.

The `gh` shim in `claude-code-web-config` does the two awkward parts inside a Claude Code web session. It accepts `GH_HOST=https://github-state-mirror.pazer.io`, and it copies the session's PAT into `GH_ENTERPRISE_TOKEN`.

## Verified

These commands ran against a locally built server behind TLS, with `GH_HOST` naming it.

- `gh api user` and `gh api repos/PazerOP/dummy-repo-pazerop` answer 200. The second call reports `X-Gsm-Cache: hit`.
- `gh auth status` reports the account. It asks GraphQL for `viewer{login}`.
- `gh pr list -R wow-look-at-my/github-state-mirror` and `gh repo view --json` both answer. Both are GraphQL.

The server log records the paths gh really calls: `/api/v3/user`, `/api/v3/` and `/api/graphql`.
