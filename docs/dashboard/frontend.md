# The dashboard front-end

Extracted from CLAUDE.md, which now carries a pointer here.

## One source, and the build output is not it

The dashboard's ONLY source is `internal/api/web/src/*.ts`. `npm run build` (tsc) compiles it to `internal/api/web/assets/*.js`. That JS is a **gitignored build output that is never committed**, embedded through `//go:embed`.

Work only in the `.ts`. The `.js` is disposable, regenerated on demand, never hand-edited, and never added to git.

The embed names each `.js` file. A missing one is therefore a Go COMPILE error. `npm run build` must run before any Go build or test. CI's `build` job runs `npm ci && npm run build` ahead of `go-toolchain` for exactly that reason.

## The gates

CI's `web-check` job is the type-check gate. It runs `npm ci` and `npm run build` on the locked toolchain, and tsc exits non-zero on any error.

The same job FAILS if any generated `.js` is tracked: `git ls-files internal/api/web/assets/*.js` must come back empty. A second, staleable copy of the front-end thus can never re-enter the tree.

Hand-written `assets/style.css` stays tracked. `assets/demo-data.js` is preview-only and NOT embedded. The CI `preview` job injects it to deploy a backend-free styling preview to buildhost, per branch.

## Modules, and the three places an asset is wired

Each `src/*.ts` emits its own standalone ES module, loaded by its own `<script type="module">` tag in `index.html`. `rate-meter.ts` self-registers the `<rate-meter>` web component. `timeline.ts` self-registers `<gsm-timeline>`. Types for its buildhost-URL component import live in the interim ambient shim `src/js-snippets-timeline.d.ts`, which is types only. Keep that shim minimal.

Adding an asset file means wiring it in **three** places. The first is the `//go:embed` and hashed-URL machinery in `internal/api/dashboard.go`. The second is the `index.html` script tag. The third is the `preview` job's copied-assets list in `ci.yml`.

## The two browser harnesses

`internal/api/testdata/browsercheck.ts` is the real-browser end-to-end frame check. It is TypeScript too. `npm run check:harness` type-checks it against `src/` (`tsconfig.harness.json`, noEmit). node runs the `.ts` directly, so there is no build step and nothing lands in `assets/`. CI's `web-check` runs it, so a changed export signature fails the build instead of surfacing mid-measurement at runtime.

`testdata/dashboardcheck.test.ts` is the second harness (`npm run test:dashboard`). It serves the built dashboard the way the preview job does. It then asserts from COMPUTED STYLE that chrome the code hides with `el.hidden` is really off screen. An author `display` outranks the UA's `[hidden]` rule, which is why `style.css` carries `[hidden] { display: none !important }`, and why nothing off-browser can see the difference.

Playwright is deliberately NOT a devDependency for either harness. `testdata/playwright.d.ts` is a minimal types-only shim, so `npm ci` stays light. Run `npm i -D playwright` to actually drive a browser.
