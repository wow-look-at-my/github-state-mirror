// Is the dashboard's chrome actually HIDDEN when the code hides it?
//
// The Copy JSON menu sat open on every page for every visitor: the code hid it
// with `el.hidden`, and `.copy-json-menu { display: flex }` beat the UA's
// `[hidden] { display: none }`. Nothing off-browser can see that — the property
// was set, the attribute was on the element, and only the CASCADE disagreed.
// So the assertion has to come from a real engine's computed style.
//
// Needs playwright (deliberately not a devDependency, see CLAUDE.md) and the
// built assets. It FAILS rather than skips without them.
//
//   npm run build && npm i -D playwright && npm run test:dashboard
import test from "node:test";
import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import { spawn, type ChildProcess } from "node:child_process";
import { createHmac, randomUUID } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, extname } from "node:path";
import { chromium, type Browser, type Page } from "playwright";

const WEB = join(import.meta.dirname, "..", "web");
const PORT = 18099;

// demo-data.js is the preview-only canned backend; injecting it the way ci.yml's
// preview job does lets the page render with no server behind it.
const DEMO_TAG = `<script type="module" src="assets/demo-data.js"></script>`;
const APP_TAG = `<script type="module" src="assets/app.js"></script>`;

const TYPES: Record<string, string> = {
	".html": "text/html",
	".js": "text/javascript",
	".css": "text/css",
};

function serve(): Server {
	const server = createServer((req, res) => {
		const path = (req.url ?? "/").split("?")[0];
		if (path === "/") {
			const html = readFileSync(join(WEB, "index.html"), "utf8").replace(APP_TAG, `${DEMO_TAG}\n    ${APP_TAG}`);
			res.writeHead(200, { "content-type": "text/html" }).end(html);
			return;
		}
		const file = join(WEB, path.replace(/^\/+/, ""));
		if (!file.startsWith(WEB) || !existsSync(file)) {
			res.writeHead(404).end();
			return;
		}
		res.writeHead(200, { "content-type": TYPES[extname(file)] ?? "application/octet-stream" }).end(readFileSync(file));
	});
	server.listen(PORT);
	return server;
}

// displayOf reports what the ENGINE resolved, which is the only thing that
// decides whether the element is on screen. `el.hidden` reads true either way.
async function displayOf(page: Page, selector: string): Promise<string> {
	return page.$eval(selector, (el) => getComputedStyle(el).display);
}

// The Latency tab draws one histogram per kind, and the webhook kind marks the
// buckets past GitHub's timeout. Bars are sized by CSS, so only a real engine
// can say whether they are visible at all.
test("the Latency tab renders histograms and flags the timeout tail", async (t) => {
	const server = serve();
	const exe = process.env.GSM_CHROMIUM ?? "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
	let browser: Browser | undefined;
	t.after(async () => {
		await browser?.close();
		server.close();
	});
	browser = await chromium.launch(existsSync(exe) ? { executablePath: exe, args: ["--no-sandbox"] } : { args: ["--no-sandbox"] });
	const page = await browser.newPage({ viewport: { width: 1280, height: 1600 } });
	await page.goto(`http://127.0.0.1:${PORT}/#latency`);
	await page.waitForSelector(".lat-card");

	const root = "#scope-body";
	const count = (sel: string) => page.$eval(root, (el, s) => el.querySelectorAll(s as string).length, sel);
	assert.equal(await count(".lat-card"), 5, "one card per demo kind");
	const tallest = await page.$eval(root, (el) => Math.max(...Array.from(el.querySelectorAll(".lat-bar")).map((b) => b.getBoundingClientRect().height)));
	assert.ok(tallest > 50, `the tallest bar is ${tallest}px: the histogram is not drawn`);
	assert.ok(await count(".lat-col.late .lat-bar:not(.zero)") > 0, "no bucket past the 10s timeout is flagged");
	assert.equal(await count(".mini.bad"), 1, "the over-timeout count is not flagged");

	if (process.env.GSM_SCREENSHOT) await page.screenshot({ path: process.env.GSM_SCREENSHOT, fullPage: true });
});

test("the Copy JSON menu opens and closes", async (t) => {
	for (const f of ["assets/app.js", "assets/style.css", "assets/demo-data.js"]) {
		assert.ok(existsSync(join(WEB, f)), `${f} is missing — run npm run build first; without it this whole file would test nothing`);
	}

	const server = serve();
	// Same prebuilt Chromium browsercheck.ts uses, rather than downloading one.
	const exe = process.env.GSM_CHROMIUM ?? "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
	let browser: Browser | undefined;
	t.after(async () => {
		await browser?.close();
		server.close();
	});
	browser = await chromium.launch(existsSync(exe) ? { executablePath: exe, args: ["--no-sandbox"] } : { args: ["--no-sandbox"] });
	const page = await browser.newPage();
	await page.goto(`http://127.0.0.1:${PORT}/`);
	await page.waitForSelector(".copy-json-trigger");

	assert.equal(await displayOf(page, ".copy-json-menu"), "none", "menu is on screen before anyone opened it");

	await page.click(".copy-json-trigger");
	assert.equal(await displayOf(page, ".copy-json-menu"), "flex", "menu did not open on the trigger");
	assert.equal(await page.getAttribute(".copy-json-trigger", "aria-expanded"), "true");

	await page.keyboard.press("Escape");
	assert.equal(await displayOf(page, ".copy-json-menu"), "none", "Escape did not close the menu");

	await page.click(".copy-json-trigger");
	await page.click("body", { position: { x: 5, y: 5 } });
	assert.equal(await displayOf(page, ".copy-json-menu"), "none", "an outside click did not close the menu");
});

// The backend-free preview has no stream, so app.ts replays canned requests.
test("the preview's request log is filled and keeps moving", async (t) => {
	const server = serve();
	const exe = process.env.GSM_CHROMIUM ?? "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
	let browser: Browser | undefined;
	t.after(async () => {
		await browser?.close();
		server.close();
	});
	browser = await chromium.launch(existsSync(exe) ? { executablePath: exe, args: ["--no-sandbox"] } : { args: ["--no-sandbox"] });
	const page = await browser.newPage({ viewport: { width: 1400, height: 1500 } });
	await page.goto(`http://127.0.0.1:${PORT}/#requests`);
	await page.waitForSelector("gsm-request-log tr.row-toggle");
	assert.equal(await page.$eval("#live-pill", (el) => el.textContent), "Demo", "the preview must not claim to be live");
	assert.ok(await page.$eval("#req-summary", (el) => el.querySelectorAll("details.req-groups").length) >= 2, "the group tables sit above the log");

	const held = (): Promise<number> => page.$eval("gsm-request-log", (el) => (el as unknown as { model: { size: number } }).model.size);
	const first = await held();
	await until("more canned requests", held, (n) => n > first, 5000);

	if (process.env.GSM_SCREENSHOT_DEMO) await page.screenshot({ path: process.env.GSM_SCREENSHOT_DEMO, fullPage: true });
});

// ---- the live dashboard ----

const BIN = join(import.meta.dirname, "..", "..", "..", "build", "server");
const LIVE_PORT = 18097;
const LIVE = `http://127.0.0.1:${LIVE_PORT}`;
const SESSION_SECRET = "livecheck-session";
const WEBHOOK_SECRET = "livecheck-webhook";

// The server signs a session with HMAC-SHA256 (internal/auth), so the check signs one the same way.
function sessionCookie(login: string): string {
	const body = Buffer.from(JSON.stringify({ login, exp: Math.floor(Date.now() / 1000) + 3600 })).toString("base64url");
	return `gsm_session=${body}.${createHmac("sha256", SESSION_SECRET).update(body).digest("base64url")}`;
}

async function startServer(dbDir: string): Promise<ChildProcess> {
	// build/server is an APE: the kernel cannot exec it, a shell can.
	const proc = spawn("/bin/sh", [BIN], {
		env: { ...process.env, LISTEN_ADDR: `127.0.0.1:${LIVE_PORT}`, DB_PATH: join(dbDir, "cache.db"), SESSION_SECRET, WEBHOOK_SECRET },
		stdio: "ignore",
	});
	for (let i = 0; i < 100; i++) {
		try {
			if ((await fetch(`${LIVE}/.well-known/docker-updater/health`)).ok) return proc;
		} catch {
		}
		await new Promise((r) => setTimeout(r, 100));
	}
	proc.kill();
	throw new Error("the server did not start");
}

// /wp-json/* is answered locally and recorded in the request log: traffic with no GitHub behind it.
async function hit(path: string, times = 1): Promise<void> {
	await Promise.all(Array.from({ length: times }, () => fetch(`${LIVE}/wp-json/${path}`)));
}

async function sendWebhook(): Promise<void> {
	const body = JSON.stringify({ zen: "live", hook_id: 1 });
	const res = await fetch(`${LIVE}/webhook`, {
		method: "POST",
		headers: {
			"Content-Type": "application/json",
			"X-GitHub-Event": "ping",
			"X-GitHub-Delivery": randomUUID(),
			"X-Hub-Signature-256": "sha256=" + createHmac("sha256", WEBHOOK_SECRET).update(body).digest("hex"),
		},
		body,
	});
	assert.ok(res.ok, `the webhook was refused: ${res.status}`);
}

// logRows reads the log as [path, count] pairs, top row first.
function logRows(page: Page): Promise<Array<[string, string]>> {
	return page.$eval("gsm-request-log", (el) => Array.from(el.querySelectorAll("tr.row-toggle")).map((tr) => {
		const tds = tr.querySelectorAll("td");
		return [tds[6]?.textContent ?? "", tds[4]?.textContent ?? ""] as [string, string];
	}));
}

// openTab shows a tab and waits for its set-up requests to end. The Timeline
// reads its first page and then opens the stream again from that page's cursor.
async function openTab(page: Page, tab: string): Promise<void> {
	await page.goto(`${LIVE}/#${tab}`);
	await page.waitForSelector(".live-pill.live");
	if (tab === "timeline") {
		await page.waitForFunction(() => (document.querySelector("gsm-timeline") as unknown as { liveReady?: boolean } | null)?.liveReady === true, undefined, { timeout: 20000 });
	}
	await page.waitForTimeout(2500);
}

async function until<T>(what: string, read: () => Promise<T>, ok: (v: T) => boolean, timeoutMs: number): Promise<T> {
	const start = Date.now();
	let last = await read();
	while (!ok(last)) {
		if (Date.now() - start > timeoutMs) assert.fail(`${what}: not within ${timeoutMs}ms; last saw ${JSON.stringify(last)}`);
		await new Promise((r) => setTimeout(r, 25));
		last = await read();
	}
	return last;
}

test("the dashboard is pushed its updates and polls for nothing", async (t) => {
	assert.ok(existsSync(BIN), `${BIN} is missing: run go-toolchain first`);
	const dbDir = mkdtempSync(join(tmpdir(), "gsm-live-"));
	let server = await startServer(dbDir);
	const exe = process.env.GSM_CHROMIUM ?? "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
	let browser: Browser | undefined;
	t.after(async () => {
		await browser?.close();
		server.kill();
	});
	browser = await chromium.launch(existsSync(exe) ? { executablePath: exe, args: ["--no-sandbox"] } : { args: ["--no-sandbox"] });
	const page = await browser.newPage({ viewport: { width: 1400, height: 1200 } });
	await page.setExtraHTTPHeaders({ Cookie: sessionCookie("PazerOP") });
	const apiCalls: string[] = [];
	page.on("request", (req) => {
		const url = new URL(req.url());
		if (url.pathname.startsWith("/api/")) apiCalls.push(url.pathname + url.search);
	});

	await page.goto(`${LIVE}/#requests`);
	await page.waitForSelector(".live-pill.live");
	await page.waitForSelector("gsm-request-log");

	await t.test("a request shows up at once, grouped", async () => {
		const sent = Date.now();
		await hit("first", 3);
		await until("the pushed row", () => logRows(page), (r) => r.some(([p, c]) => p === "/wp-json/first" && c === "×3"), 1500);
		t.diagnostic(`request to row: ${Date.now() - sent}ms`);
		await hit("second");
		const r = await until("a new kind of request on top", () => logRows(page), (v) => v[0]?.[0] === "/wp-json/second", 1500);
		assert.deepEqual(r.map(([p]) => p), ["/wp-json/second", "/wp-json/first"]);
		await hit("first", 4);
		const after = await until("the counter", () => logRows(page), (v) => v[1]?.[1] === "×7", 1500);
		assert.equal(after[0]?.[0], "/wp-json/second", "a busy row counts up where it is and never jumps");
	});

	await t.test("an idle page makes no request at all", async () => {
		apiCalls.length = 0;
		await page.waitForTimeout(12000);
		assert.deepEqual(apiCalls, [], "the page polled while nothing was happening");
		assert.equal(await page.getAttribute("#live-pill", "class"), "live-pill live", "the heartbeat keeps the stream live");

		// A tab opens its stream, and the Timeline reads its first page. No tab may ask for more.
		for (const tab of ["mine", "all", "webhooks", "ratelimit", "latency", "timeline"]) {
			await openTab(page, tab);
			apiCalls.length = 0;
			await page.waitForTimeout(6000);
			assert.deepEqual(apiCalls, [], `the ${tab} tab kept loading with nothing happening`);
		}
		await page.goto(`${LIVE}/#requests`);
		await page.waitForSelector("gsm-request-log");
	});

	await t.test("a flood loses nothing and blocks no frame", async () => {
		await page.evaluate(() => {
			const w = window as unknown as { __long: number };
			w.__long = 0;
			new PerformanceObserver((list) => { w.__long += list.getEntries().length; }).observe({ entryTypes: ["longtask"] });
		});
		const total = 3000;
		for (let i = 0; i < total; i += 100) {
			await Promise.all(Array.from({ length: 100 }, (_, j) => hit(`flood/${(i + j) % 400}`)));
		}
		const held = (): Promise<number> => page.$eval("gsm-request-log", (el) => (el as unknown as { model: { size: number } }).model.size);
		assert.equal(await until("every event", held, (n) => n >= total + 8, 5000), total + 8);
		const worst = await page.$eval("gsm-request-log", (el) => (el as unknown as { worstRenderMs: number }).worstRenderMs);
		const long = await page.evaluate(() => (window as unknown as { __long: number }).__long);
		const drawn = await page.$eval("gsm-request-log", (el) => el.querySelectorAll("tr.row-toggle").length);
		t.diagnostic(`worst render ${worst.toFixed(1)}ms, long tasks ${long}, rows in the DOM ${drawn}`);
		assert.ok(worst < 50, `one render of the log took ${worst.toFixed(1)}ms`);
		assert.equal(long, 0, "the flood produced a task over 50ms");
		assert.equal(drawn, 100, "the DOM holds one page of rows however long the log is");
	});

	await t.test("pause holds the view and the filter narrows it", async () => {
		await page.click(".reqlog-pause");
		const before = await logRows(page);
		await hit("while-paused", 5);
		await until("the held count", () => page.$eval(".reqlog-pause", (el) => el.textContent ?? ""), (s) => s === "Resume (+5)", 1500);
		assert.deepEqual(await logRows(page), before, "a paused log moved");
		await page.click(".reqlog-pause");
		await until("the resumed row", () => logRows(page), (r) => r[0]?.[0] === "/wp-json/while-paused" && r[0]?.[1] === "×5", 1500);

		await page.fill(".reqlog-filter", "wp-json/first");
		const r = await until("the filtered log", () => logRows(page), (v) => v.length === 1, 1500);
		assert.deepEqual(r, [["/wp-json/first", "×7"]]);
		await page.fill(".reqlog-filter", "");
	});

	await t.test("the Webhooks tab hears a delivery", async () => {
		await page.goto(`${LIVE}/#webhooks`);
		await page.waitForSelector(".empty");
		await sendWebhook();
		await until("the delivery row", () => page.$eval("#scope-body", (el) => el.querySelectorAll("table.cols-webhooks tr.row-toggle").length), (n) => n === 1, 2500);
	});

	// The stream a tab opens is the request it makes. Traffic must add none: a
	// page that reads its tab again on each change is a poll under steady
	// traffic.
	await t.test("a busy page is pushed its data and makes no request", async () => {
		const rows = (): Promise<number> => page.$eval("#scope-body", (el) => el.querySelectorAll("table.cols-webhooks tr.row-toggle").length);
		const handled = (): Promise<number> => page.$eval("#scope-body", (el) => Number(el.querySelector(".lat-card .mini .n")?.textContent ?? "0"));
		for (const tab of ["mine", "all", "webhooks", "requests", "ratelimit", "latency", "timeline"]) {
			await openTab(page, tab);
			const before = tab === "webhooks" ? await rows() : tab === "latency" ? await handled() : 0;
			apiCalls.length = 0;
			for (let i = 0; i < 12; i++) {
				await Promise.all([hit(`busy/${tab}/${i}`), sendWebhook()]);
				await page.waitForTimeout(250);
			}
			if (tab === "webhooks") await until("each delivery's row", rows, (n) => n === before + 12, 2500);
			if (tab === "latency") await until("each delivery's count", handled, (n) => n === before + 12, 2500);
			await page.waitForTimeout(1500);
			assert.deepEqual(apiCalls, [], `the ${tab} tab made a request while traffic flowed`);
		}
	});

	await t.test("a dead server is reported and a new one is picked up", async () => {
		await page.goto(`${LIVE}/#requests`);
		await page.waitForSelector("gsm-request-log tr.row-toggle");
		server.kill();
		await page.waitForSelector(".live-pill.down");
		server = await startServer(dbDir);
		await page.waitForSelector(".live-pill.live");
		await hit("after-restart");
		const r = await until("the new server's log", () => logRows(page), (v) => v.length === 1, 3000);
		assert.deepEqual(r, [["/wp-json/after-restart", "×1"]], "the old process's log must not mix with the new one");
	});

	if (process.env.GSM_SCREENSHOT) await page.screenshot({ path: process.env.GSM_SCREENSHOT, fullPage: true });
});
