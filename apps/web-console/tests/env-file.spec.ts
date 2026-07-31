import { execFileSync, spawn, spawnSync } from "node:child_process";
import { randomBytes, scryptSync, timingSafeEqual } from "node:crypto";
import { appendFileSync, copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { expect, test } from "@playwright/test";

import { API_KEY, CONSOLE_PASSWORD, UPSTREAM } from "./constants";

// THE SETUP PATH ITSELF, WHICH NO TEST EXERCISED (E29).
//
// The hash was proven only through `playwright.config.ts`'s `webServer.env`: the config runs the shipped
// script and hands the value STRAIGHT to a process. That is not the path any operator takes. Both
// `docs/operations/console.md` and `README.md` tell them to put the value in `.env.local` — and `.env.local`
// is gitignored, so a fresh clone or a git worktree has none, and the whole suite is green without it.
// The one path the documentation prescribes was the one path nothing ran.
//
// What was on the other side of that hole, measured on this box with Next's own loader (2026-07-31):
// `scrypt$16384$8$1$<salt>$<key>` is 83 characters and 6 `$`-parts on disk, and `@next/env` handed the
// console 10 characters and 1 part. `$16384`, `$8`, `$1` and the leading run of the salt are read as
// variable references and expand to nothing. The console then answers `503 console_not_configured` —
// "is not set (or is not a valid scrypt hash)" — which sends the operator hunting for a variable that IS set.
//
// So this file drives the DOCUMENTED path end to end: the shipped script, a real `.env.local`, and the real
// consuming loader.
const APP_DIR = process.cwd();
const SCRIPT = resolve(APP_DIR, "scripts/hash-password.mjs");
const PREFIX = "PALAI_CONSOLE_PASSWORD_HASH=";

test("a hash appended to .env.local survives the loader the console reads it with", () => {
  // A cwd change must FAIL here rather than quietly measure nothing — tests/constants.ts makes the same
  // assumption implicitly and this is the assertion that keeps it honest.
  expect(existsSync(SCRIPT), `scripts/hash-password.mjs is not under ${APP_DIR} — this test would prove nothing`).toBe(true);

  const dir = mkdtempSync(join(tmpdir(), "palai-console-env-"));
  try {
    // The recipe the docs carried until this was fixed — the script's stdout appended to .env.local:
    //   printf %s '…' | node scripts/hash-password.mjs >> .env.local
    // It is the shape that broke, so it stays asserted. §2 now leads with `--write` (the test below), but an
    // operator's fingers and every older copy of these instructions still produce this, and it must work.
    const printed = execFileSync(process.execPath, [SCRIPT], { cwd: APP_DIR, input: CONSOLE_PASSWORD, encoding: "utf8" });
    appendFileSync(join(dir, ".env.local"), printed);

    const onDisk = readFileSync(join(dir, ".env.local"), "utf8").trim().slice(PREFIX.length);
    const loaded = loadThroughNextEnv(dir);

    expect(loaded, "@next/env produced no PALAI_CONSOLE_PASSWORD_HASH at all from a file that has the line").not.toBe(undefined);
    // Shape first, so a failure PRINTS the corruption instead of dumping bytes. On the RED run of this test
    // it read `83/6 → 38/1`; the surviving length varies with the salt, because how much of it is eaten
    // depends on where its first non-`[A-Za-z0-9_]` character falls. The 6 → 1 is the invariant.
    expect(
      { characters: loaded!.length, parts: loaded!.split(SEPARATORS).length },
      "the loader changed the value's shape — an env-file reader expanded part of the hash",
    ).toEqual({ characters: onDisk.length, parts: onDisk.split(SEPARATORS).length });
    expect(loaded === onDisk, "the loaded value is not the value on disk").toBe(true);
    expect(verifiesFixturePassword(loaded!), "the loaded value is not a hash of the password the script was given").toBe(true);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// THE DOCUMENTED COMMAND, RUN WHERE ITS DEFAULT PATH IS SAFE TO TEST.
//
// `--write` resolves `.env.local` from the SCRIPT's own location, not the cwd, because the docs invoke it
// from the repo root as often as from apps/web-console. Testing that resolution therefore means moving the
// script — which costs nothing, since the whole point of this file is that it is one dependency-free file an
// operator can read. It is copied, not re-implemented, so what runs here is the shipped bytes.
//
// It must not be run in place: writing apps/web-console/.env.local would hand a password hash to the
// UNCONFIGURED console playwright.config.ts starts on PALAI_CONSOLE_UNCONFIGURED_PORT, and tests/auth.spec.ts's
// fail-closed proof would quietly stop proving anything. The last assertion in this test is that it didn't.
test("the documented command lands one working hash in .env.local, and running it again replaces rather than appends", () => {
  const appEnvBefore = snapshot(resolve(APP_DIR, ".env.local"));
  const dir = mkdtempSync(join(tmpdir(), "palai-console-env-"));
  try {
    mkdirSync(join(dir, "scripts"));
    copyFileSync(SCRIPT, join(dir, "scripts", "hash-password.mjs"));

    // The state an operator who followed the OLD docs is actually in: an unrelated variable they need kept,
    // and a `$`-separated line the loader has been eating. Both are what `--write` has to deal with.
    const unrelated = "PALAI_BASE_URL=http://127.0.0.1:8080";
    writeFileSync(join(dir, ".env.local"), `${unrelated}\n${PREFIX}${legacyDollarHash("some-earlier-password")}\n`);

    const first = write(dir, CONSOLE_PASSWORD);
    expect(first.stdout, "--write printed to stdout — a credential-derived value must not land in a pipeline or a log").toBe("");
    expect(first.stderr.includes("scrypt"), "--write printed the hash on stderr").toBe(false);

    const lines = readFileSync(join(dir, ".env.local"), "utf8").split("\n").filter((l) => l !== "");
    expect(lines.filter((l) => l.startsWith(PREFIX)).length, "the file carries more than one hash — the reader gets to choose").toBe(1);
    expect(lines.includes(unrelated), "--write dropped an unrelated line; this file also holds PALAI_API_KEY").toBe(true);
    expect(statSync(join(dir, ".env.local")).mode & 0o777, ".env.local is readable beyond its owner").toBe(0o600);

    const loaded = loadThroughNextEnv(dir);
    expect(loaded, "@next/env produced no PALAI_CONSOLE_PASSWORD_HASH from the file --write just wrote").not.toBe(undefined);
    expect(verifiesFixturePassword(loaded!), "the console would not accept what one documented command left behind").toBe(true);

    // A SECOND run, with a DIFFERENT password: the surviving line must be the new one, and there must still
    // be exactly one. Appending could satisfy neither.
    const second = "console-proof-operator-password-DO-NOT-REUSE-second-8e3d1f7a";
    write(dir, second);
    const after = readFileSync(join(dir, ".env.local"), "utf8").split("\n").filter((l) => l.startsWith(PREFIX));
    expect(after.length, "a second run appended instead of replacing").toBe(1);
    const reloaded = loadThroughNextEnv(dir);
    expect(verifiesFixturePassword(reloaded!, second), "the surviving hash is not the password the last run was given").toBe(true);
    expect(verifiesFixturePassword(reloaded!), "the FIRST password still opens the console after it was replaced").toBe(false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
  expect(snapshot(resolve(APP_DIR, ".env.local")), "the test wrote apps/web-console/.env.local — the fail-closed console in auth.spec.ts would stop being unconfigured").toBe(appEnvBefore);
});

// A LEGACY `$` HASH IS REFUSED, AND THE REFUSAL SAYS WHY — THE COMPATIBILITY CLAIM IS WITHDRAWN.
//
// This test asserted the opposite yesterday and PASSED, and how it passed is the finding. `lib/session.ts`
// splits on `[.$]`, so the parse handles both forms; the value never reaches the parse. Measured 2026-08-01
// with `@next/env`, the loader this console actually runs under:
//
//   PALAI_CONSOLE_PASSWORD_HASH='scrypt$16384$8$1$SALT$KEY'  →  loadEnvConfig(dir)  →  'scrypt6384'
//
// dotenv-expand rewrites values ALREADY IN process.env, not only the ones it reads out of a file — and it
// runs whenever any `.env` file is present. A git worktree has none, which is where the old version of this
// test ran and why it was green. **Its green was a property of the harness, not of the product**, on the one
// surface whose entire purpose is the path an operator actually walks. That is this tree's recurring defect
// landing on the fix written to cure it.
//
// The value is DESTROYED rather than reshaped, so nothing can be compatible with it. What is left is a
// recognisable signature, and the console owes the operator the reason rather than a generic "not set"
// about a variable they can see is set.
//
// The wrong-password arm is not decoration: without it, a console that accepted anything would pass.
test("a `$`-separated hash cannot survive the loader, and the console says so when it arrives destroyed", async () => {
  // TWO DETERMINISTIC ARMS, BECAUSE THE ONE-ARM VERSION OF THIS TEST DEPENDED ON ITS OWN ENVIRONMENT AND
  // I WROTE BOTH OF THEM. The first said a legacy `$` hash still WORKS, and passed only where no `.env`
  // file exists; the second said it is REFUSED, and passed only where one does. Neither was wrong about
  // the console — both were measuring the machine. Inverting a harness dependency is not removing it.
  //
  // So neither arm below spawns a console with a `$` value and asks what happens. The first measures the
  // LOADER against a file it creates itself, in a directory it owns. The second hands the console a value
  // that is ALREADY destroyed, which is deterministic everywhere.

  // ARM ONE: the destruction, measured against @next/env in a directory this test controls.
  const dir = mkdtempSync(join(tmpdir(), "palai-legacy-"));
  try {
    const legacy = legacyDollarHash(CONSOLE_PASSWORD);
    writeFileSync(join(dir, ".env.local"), `${PREFIX}${legacy}\n`, { mode: 0o600 });
    const loaded = loadThroughNextEnv(dir);
    expect(loaded, "@next/env produced nothing at all from a file that has a hash in it").not.toBe(undefined);
    expect(loaded, "a `$` hash survived @next/env — then dotenv-expand changed behaviour and the dot separator is no longer load-bearing").not.toBe(legacy);
    expect((loaded ?? "").includes("$"), `the loader left a "$" in the value: ${String(loaded)}`).toBe(false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }

  // ARM TWO: what the console says when the destroyed value reaches it. `scrypt6384` is the measured
  // remains of `scrypt$16384$8$1$…` — the prefix survives, every separator and everything they delimited
  // does not. The operator can SEE the variable is set, so "not set" would send them hunting; the refusal
  // has to name the cause and the one command that repairs it.
  const port = await freePort();
  const base = `http://127.0.0.1:${port}`;
  const child = spawn(resolve(APP_DIR, "node_modules/.bin/next"), ["start", "-p", String(port)], {
    cwd: APP_DIR,
    env: { ...process.env, PALAI_API_KEY: API_KEY, PALAI_BASE_URL: UPSTREAM, PALAI_CONSOLE_PASSWORD_HASH: "scrypt6384" },
    stdio: "ignore",
  });
  try {
    await waitForConsole(base);
    expect(await postPassword(base, CONSOLE_PASSWORD), "a console holding a destroyed hash opened for a password").toBe(401);

    const relay = await fetch(`${base}/api/palai/v1/agents`);
    expect(relay.status, "the relay must answer 503 console_not_configured — this console has no usable credential at all").toBe(503);
    const detail = ((await relay.json()) as { detail?: string }).detail ?? "";
    expect(detail.includes("separators stripped"), `the refusal did not name the cause: ${detail}`).toBe(true);
    expect(detail.includes("--write"), `the refusal did not name the command that repairs it: ${detail}`).toBe(true);
  } finally {
    child.kill("SIGTERM");
  }
});

// Both separators, because a hash generated before this changed is still `$`-separated and must keep working.
const SEPARATORS = /[.$]/;

/**
 * loadThroughNextEnv reads `dir/.env.local` with THE CONSUMING LOADER — `@next/env`, the module `next start`
 * uses — and returns what it put in the environment.
 *
 * IN ITS OWN PROCESS, and that is not incidental. `loadEnvConfig` is one-shot: it stamps
 * `process.env.__NEXT_PROCESSED_ENV` and every later call in the same process returns without opening a file.
 * Playwright's process has already loaded an env, so an in-process call would report `undefined` and a
 * carelessly written assertion would pass over nothing.
 *
 * `NODE_ENV=production` is stamped for a second reason of the same kind: `@next/env` deliberately SKIPS
 * `.env.local` when `NODE_ENV === "test"`, and a Playwright child could plausibly inherit that. Production is
 * also what `next start` — the console the suite actually drives — runs as.
 *
 * A HAND-ROLLED PARSER WOULD NOT HAVE FOUND THIS. Splitting the file on `=` yields the correct 83 characters;
 * the expansion is the loader's, so only the loader can be the witness.
 */
function loadThroughNextEnv(dir: string): string | undefined {
  const probe = join(dir, "load-probe.cjs");
  writeFileSync(
    probe,
    [
      `const nextPkg = require.resolve("next/package.json", { paths: [${JSON.stringify(APP_DIR)}] });`,
      `const { loadEnvConfig } = require(require.resolve("@next/env", { paths: [nextPkg] }));`,
      `loadEnvConfig(${JSON.stringify(dir)}, false);`,
      `const v = process.env.PALAI_CONSOLE_PASSWORD_HASH;`,
      `process.stdout.write(v === undefined ? "" : v);`,
    ].join("\n"),
  );
  const out = execFileSync(process.execPath, [probe], {
    encoding: "utf8",
    env: { ...process.env, NODE_ENV: "production", PALAI_CONSOLE_PASSWORD_HASH: undefined },
  });
  return out === "" ? undefined : out;
}

/**
 * verifiesFixturePassword is what "a hash the console accepts" MEANS: the fixture password, run through the
 * parameters and salt carried in this value, reproduces its key.
 *
 * It repeats the derivation rather than importing `lib/session.ts` because that module opens with
 * `import "server-only"`, whose default entry throws outside a React Server Component — a Playwright spec
 * cannot import `parseHash` or `verifyPassword` at all. Repeating it is the stronger assertion anyway:
 * "parses" would accept a well-shaped hash of some OTHER password. The binding to the real reader is the
 * suite's own sign-in — playwright.config.ts derives the running console's hash from this same script, and
 * tests/auth.spec.ts opens the door with it.
 */
function verifiesFixturePassword(hash: string, password: string = CONSOLE_PASSWORD): boolean {
  const parts = hash.split(SEPARATORS);
  if (parts.length !== 6 || parts[0] !== "scrypt") return false;
  const [N, r, p] = [Number(parts[1]), Number(parts[2]), Number(parts[3])];
  if (!Number.isInteger(N) || !Number.isInteger(r) || !Number.isInteger(p) || N < 2 || r < 1 || p < 1) return false;
  const salt = Buffer.from(parts[4], "base64url");
  const key = Buffer.from(parts[5], "base64url");
  if (salt.length < 16 || key.length < 16) return false;
  const derived = scryptSync(password, salt, key.length, { N, r, p });
  return derived.length === key.length && timingSafeEqual(derived, key);
}

/**
 * write runs the copied script's `--write` mode with the password on stdin and returns BOTH streams —
 * spawnSync rather than execFileSync, because execFileSync returns stdout alone and the claim being asserted
 * is about what does NOT appear on either.
 */
function write(dir: string, password: string): { stdout: string; stderr: string } {
  const run = spawnSync(process.execPath, [join(dir, "scripts", "hash-password.mjs"), "--write"], {
    input: password,
    encoding: "utf8",
  });
  expect(run.status, `the script exited ${run.status}: ${run.stderr}`).toBe(0);
  return { stdout: run.stdout, stderr: run.stderr };
}

/**
 * legacyDollarHash builds the format as it was BEFORE this change: `scrypt$N$r$p$salt$key`, with exactly the
 * parameters scripts/hash-password.mjs used. It is written out rather than produced by an old copy of the
 * script, because there is no old copy to run — and the shape is the whole content of the compatibility claim.
 */
function legacyDollarHash(password: string): string {
  const salt = randomBytes(16);
  const key = scryptSync(password, salt, 32, { N: 16384, r: 8, p: 1 });
  return `scrypt$16384$8$1$${salt.toString("base64url")}$${key.toString("base64url")}`;
}

/** snapshot returns a file's bytes, or null when it does not exist — enough to prove a test left it alone. */
function snapshot(path: string): string | null {
  return existsSync(path) ? readFileSync(path, "utf8") : null;
}

/** freePort asks the OS for one, so this file can spawn a console without colliding with a fixed port. */
function freePort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const server = createServer();
    server.on("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address() as { port: number };
      server.close(() => resolvePort(port));
    });
  });
}

async function waitForConsole(base: string): Promise<void> {
  const deadline = Date.now() + 60_000;
  for (;;) {
    try {
      const res = await fetch(`${base}/login`);
      if (res.ok) return;
    } catch {
      // not listening yet
    }
    if (Date.now() > deadline) throw new Error(`the spawned console never served ${base}/login`);
    await new Promise((r) => setTimeout(r, 250));
  }
}

/** postPassword drives the REAL login route — Origin included, because lib/session.ts refuses a POST without one. */
async function postPassword(base: string, password: string): Promise<number> {
  const res = await fetch(`${base}/api/console/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Origin: base },
    body: JSON.stringify({ password }),
  });
  return res.status;
}
