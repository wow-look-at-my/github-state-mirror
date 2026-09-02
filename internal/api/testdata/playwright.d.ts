// Minimal ambient declaration for the slice of Playwright browsercheck.ts uses.
//
// Playwright is NOT a committed devDependency on purpose: CI never runs the
// browser harness (it needs a real Chromium), so making every `npm ci` pull a
// multi-megabyte package to type-check one file is a bad trade. This shim is
// the same interim-shim pattern as src/js-snippets-timeline.d.ts — types only,
// kept to exactly what is used, so it stays cheap to keep honest.
//
// Run the harness with `npm i -D playwright` in the working tree; the real
// types are a superset of this and the harness keeps compiling.
declare module "playwright" {
    interface ConsoleMessage {
        type(): string;
        text(): string;
    }

    interface Request {
        url(): string;
    }

    interface Route {
        request(): Request;
        fulfill(options: {
            status?: number;
            contentType?: string;
            body?: string | Buffer;
        }): Promise<void>;
    }

    interface Page {
        on(event: "console", handler: (msg: ConsoleMessage) => void): void;
        route(url: string, handler: (route: Route) => unknown): Promise<void>;
        goto(url: string): Promise<unknown>;
        waitForFunction(fn: () => unknown, arg?: unknown, options?: { timeout?: number }): Promise<unknown>;
        waitForTimeout(ms: number): Promise<void>;
        evaluate<T>(fn: () => T | Promise<T>): Promise<T>;
        $eval<T>(selector: string, fn: (el: Element) => T): Promise<T>;
        click(selector: string, options?: { position?: { x: number; y: number } }): Promise<void>;
        getAttribute(selector: string, name: string): Promise<string | null>;
        keyboard: { press(key: string): Promise<void> };
        screenshot(options: { path: string; fullPage?: boolean }): Promise<Buffer>;
        waitForSelector(selector: string): Promise<unknown>;
    }

    interface Browser {
        newPage(options?: { viewport?: { width: number; height: number } }): Promise<Page>;
        close(): Promise<void>;
    }

    interface BrowserType {
        launch(options?: { executablePath?: string; args?: string[] }): Promise<Browser>;
    }

    const chromium: BrowserType;
}
