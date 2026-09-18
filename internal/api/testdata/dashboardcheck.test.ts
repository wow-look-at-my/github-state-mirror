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
import { existsSync, readFileSync } from "node:fs";
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
