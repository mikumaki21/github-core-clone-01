// Entry for github-core-clone-01 (Go port), built by Marley (Muse) for @mikuma17
// in the 2026-11-20 session. Thin launcher: it stages the statically linked
// Go binary `forge` from this directory into the writable temp dir (the
// trial mounts /bot read-only and the committed binary may not carry the
// exec bit, so it cannot run in place), spawns the staged copy, reads the
// ephemeral port the binary bound, and then runs the starter lifecycle
// exactly as the Node entry did: register the port with the env, poll until
// the env says the trial is over, kill the binary and exit 0. The staged
// copy is byte-identical to the committed binary and its path never enters
// any response, so replay stays deterministic.
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing to
//      talk to, so the program exits 1 at once.
//   2. The binary binds 127.0.0.1:0 (the kernel picks a free port) and prints
//      "[forge] listening on port N". Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The first register wins; a
//      409 means another entry already registered, and this process keeps
//      waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}.
//   5. Kill the binary and exit 0.

import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { createInterface } from 'node:readline';
import { chmodSync, copyFileSync } from 'node:fs';
import { tmpdir } from 'node:os';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

/** Stage ./forge into the temp dir and spawn it; resolve with its bound port once it prints it. */
function startForge(): Promise<{ child: ChildProcess; port: number }> {
  const dir = dirname(fileURLToPath(import.meta.url));
  const staged = join(tmpdir(), `forge-${process.pid}`);
  copyFileSync(join(dir, 'forge'), staged);
  chmodSync(staged, 0o755);
  const child = spawn(staged, [], { env: process.env, stdio: ['ignore', 'pipe', 'inherit'] });
  return new Promise((resolve, reject) => {
    const lines = createInterface({ input: child.stdout! });
    const timer = setTimeout(() => reject(new Error('forge did not print its port in time')), 10_000);
    lines.on('line', (line) => {
      const m = /\[forge\] listening on port (\d+)/.exec(line);
      if (m) {
        clearTimeout(timer);
        resolve({ child, port: Number(m[1]) });
      }
    });
    child.once('error', (err) => {
      clearTimeout(timer);
      reject(err);
    });
    child.once('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`forge exited early with code ${code}`));
    });
  });
}

async function main(): Promise<void> {
  const { child, port } = await startForge();
  process.stdout.write(`[entry] forge listening on port ${port}\n`);
  try {
    await register(port);
    process.stdout.write('[entry] registered; waiting for the env to say done\n');
    await waitForDone();
  } finally {
    child.kill('SIGTERM');
    await new Promise<void>((resolve) => {
      const timer = setTimeout(() => {
        try {
          child.kill('SIGKILL');
        } catch {
          // already gone
        }
        resolve();
      }, 2_000);
      child.once('exit', () => {
        clearTimeout(timer);
        resolve();
      });
    });
  }
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
