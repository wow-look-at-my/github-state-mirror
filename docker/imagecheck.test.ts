// Does the IMAGE start and serve? Nothing used to ask.
import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const IMAGE = 'gsm-imagecheck:local';
const PROBE = 'gsm-imagecheck-probe';
const PORT = 18080;
const REPO_ROOT = join(import.meta.dirname, '..');

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

// APE_MAGIC opens every APE. A kernel that has no handler for it refuses the file.
const APE_MAGIC = 'MZqFpD';

// kernelExecsAPEs asks execve itself, in a container from the image. A
// binfmt_misc listing is no answer: a runner without binfmt_misc mounted lists
// nothing. The kernel still applies the host's entries to its containers. The
// probe is no host process, because glibc's execvp runs a file it gets
// ENOEXEC for under /bin/sh. busybox env in the image has no such fallback.
function kernelExecsAPEs(): boolean {
  const r = docker(
    [
      'run', '--rm', '--entrypoint', '/bin/sh', IMAGE, '-c',
      `printf "${APE_MAGIC}='\\n'\\nexit 0\\n" > /tmp/apeprobe && chmod 700 /tmp/apeprobe && exec /bin/env /tmp/apeprobe`,
    ],
    true,
  );
  if (r.status === 0) return true;
  if (r.status === 126 && r.stderr.includes('Exec format error')) return false;
  throw new Error(`cannot tell whether this kernel execs an APE: the probe exited ${r.status}\n${r.stderr}`);
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
    existsSync(join(REPO_ROOT, 'build/server')),
    'build/server is missing — run go-toolchain first; without it this whole file would test nothing',
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
// merely happens to work. An APE is not an ELF, so a kernel with no APE
// binfmt_misc entry refuses the execve() of an exec-form entrypoint.
test('an exec-form entrypoint cannot start this image', { timeout: 180_000 }, async (t) => {
  // The image's binary must be an APE, not an ELF: that is the whole reason execve() refuses it.
  const head = readFileSync(join(REPO_ROOT, 'build/server')).subarray(0, 8).toString('latin1');
  assert.ok(head.startsWith(APE_MAGIC), `build/server starts ${JSON.stringify(head)}, not the APE header ${APE_MAGIC}`);

  const hostHandler = kernelExecsAPEs();
  t.diagnostic(`this kernel ${hostHandler ? 'has' : 'has no'} APE binfmt_misc entry`);

  // A user namespace with its own empty binfmt_misc is the kernel of a host with
  // no APE entry, on any host. busybox env execs with no shell fallback.
  const bare = docker(
    [
      'run', '--rm', '--privileged', '--user', '0', '--entrypoint', '/bin/unshare', IMAGE,
      '-U', '-r', '-m', '/bin/sh', '-c',
      'mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc && exec /bin/env /usr/local/bin/github-state-mirror',
    ],
    true,
  );
  assert.ok(
    bare.status === 126 && bare.stderr.includes('Exec format error'),
    `with no APE binfmt_misc entry, execve of the image's binary exited ${bare.status}, not 126 with ENOEXEC — ` +
      `re-derive why /bin/sh is named:\n${bare.stderr}`,
  );

  // The literal exec-form ENTRYPOINT.
  if (hostHandler) return;
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
    'an exec-form entrypoint started this image on a kernel with no APE entry, so the ENTRYPOINT no longer proves anything — re-derive why /bin/sh is named',
  );
});
