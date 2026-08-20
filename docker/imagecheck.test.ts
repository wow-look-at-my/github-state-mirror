// Does the IMAGE start and serve? Nothing used to ask.
//
// go-toolchain tests the host binary and publish-ghcr pushes without running
// what it built, so the first execution of the entrypoint was production. An
// image whose binary the kernel could not exec shipped green, and every route
// 404'd for the whole fleet that routes its GitHub traffic through here.
//
// Needs docker and build/server_cosmo_fat (go-toolchain's output). It FAILS
// rather than skips when either is missing: a check that quietly passes on a
// machine without docker is how this got shipped in the first place.
//
//   go-toolchain && npm run test:image
import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const IMAGE = 'gsm-imagecheck:local';
const PROBE = 'gsm-imagecheck-probe';
const PORT = 18080;
const REPO_ROOT = join(import.meta.dirname, '..');

// Both are registered unconditionally, so a 404 on either means the process
// answering is not this program. Both 404'd during the outage.
const ROUTES = ['/.well-known/docker-updater/health', '/'];

function docker(args: string[], allowFail = false): { status: number; stdout: string; stderr: string } {
  const r = spawnSync('docker', args, { encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 });
  if (r.error) {
    throw new Error(`cannot run docker (${r.error.message}) — this check needs a working docker daemon`);
  }
  if (r.status !== 0 && !allowFail) {
    throw new Error(`docker ${args.join(' ')} exited ${r.status}\n${r.stderr}`);
  }
  return { status: r.status ?? -1, stdout: r.stdout, stderr: r.stderr };
}

function removeContainer(name: string): void {
  docker(['rm', '-f', name], true);
}

// Resolves to the first status the route answers, or null if it never answers.
async function probeRoute(route: string, seconds: number): Promise<number | null> {
  for (let i = 0; i < seconds; i++) {
    try {
      const res = await fetch(`http://127.0.0.1:${PORT}${route}`);
      return res.status;
    } catch {
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
  return null;
}

test('the binary the image needs is present', () => {
  assert.ok(
    existsSync(join(REPO_ROOT, 'build/server_cosmo_fat')),
    'build/server_cosmo_fat is missing — run go-toolchain first; without it this whole file would test nothing',
  );
});

test('the image builds', { timeout: 600_000 }, () => {
  docker(['build', '-t', IMAGE, REPO_ROOT]);
});

test('a container from the image serves its own routes', { timeout: 180_000 }, async (t) => {
  removeContainer(PROBE);
  t.after(() => {
    docker(['logs', PROBE], true);
    removeContainer(PROBE);
  });
  docker(['run', '-d', '--name', PROBE, '-p', `${PORT}:8080`, IMAGE]);

  for (const route of ROUTES) {
    const status = await probeRoute(route, 60);
    assert.notEqual(status, null, `${route} never answered — the container is not serving (it most likely never started)`);
    assert.notEqual(status, 404, `${route} answered 404 — the process answering is not this program's router`);
  }

  const running = docker(['inspect', '-f', '{{.State.Running}}', PROBE]).stdout.trim();
  assert.equal(running, 'true', 'the container stopped after answering — it is not stable');
});

// The negative control. Without it the checks above pass for an image that
// merely happens to work, and nothing states WHY the entrypoint is written the
// way it is. An APE is not an ELF, so the exec form's execve() cannot start it,
// and no amount of shell present in the image changes that.
test('an exec-form entrypoint cannot start this image', { timeout: 180_000 }, async (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'gsm-execform-'));
  const name = `${PROBE}-execform`;
  writeFileSync(join(dir, 'Dockerfile'), `FROM ${IMAGE}\nENTRYPOINT ["/usr/local/bin/github-state-mirror"]\n`);
  removeContainer(name);
  t.after(() => removeContainer(name));

  docker(['build', '-t', `${IMAGE}-execform`, dir]);
  docker(['run', '-d', '--name', name, '-p', `${PORT + 1}:8080`, `${IMAGE}-execform`], true);

  await new Promise((r) => setTimeout(r, 3000));
  const running = docker(['inspect', '-f', '{{.State.Running}}', name], true).stdout.trim();
  assert.notEqual(
    running,
    'true',
    'an exec-form entrypoint started this image, so the ENTRYPOINT no longer proves anything — re-derive why /bin/sh is named',
  );
});
