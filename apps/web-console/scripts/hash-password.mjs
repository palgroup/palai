#!/usr/bin/env node
// Turn a console password into the value of PALAI_CONSOLE_PASSWORD_HASH. One file, zero dependencies, and
// the password is read from STDIN — never from argv, where it would be visible in `ps`, in a shell history
// file and in any process listing the machine keeps.
//
//   printf %s 'a-long-console-password' | node scripts/hash-password.mjs --write   # → apps/web-console/.env.local
//   printf %s 'a-long-console-password' | node scripts/hash-password.mjs           # → stdout, for anywhere else
//
// The output format is `scrypt.N.r.p.salt.key`, base64URL and unpadded, so the whole line contains exactly
// one `=` (the env var's own), no `$`, and nothing any reader expands. lib/session.ts only ever READS this
// format; this script is the only thing that writes it. They are deliberately not coupled by an import —
// this file has to stay something an operator can read end to end before trusting it with a password — so
// the binding between them is a test: tests/auth.spec.ts signs in with a password whose hash came out of
// this script, and tests/env-file.spec.ts carries one through a real .env.local and Next's own loader.
//
// TWO THINGS HERE ARE REPAIRS, and both were the same bug (E29):
//
//  1. THE SEPARATOR WAS `$`, AND `>> .env.local` SILENTLY DESTROYED THE HASH. Every dotenv reader expands
//     `$`. Measured with @next/env on 2026-07-31: 83 characters and 6 parts on disk, 38 characters and 1
//     part in the console's environment — `$16384`, `$8`, `$1` and the head of the salt read as variable
//     names. The console answered `503 console_not_configured` — "is not set (or is not a valid scrypt
//     hash)" — about a variable that WAS set, which is an hour of looking for the wrong thing. Escaping
//     each `$` fixes dotenv and `set -a; . .env.local`, and then breaks `node --env-file` and the
//     single-quoted `NAME='…'` export this repo's own docs print. A dot has no meaning to any of them, so
//     there is ONE form of the value and it is correct everywhere. lib/session.ts still parses `$`, so a
//     hash generated before today keeps working and nothing has to be regenerated.
//  2. `>>` CANNOT BE RUN TWICE. Appending leaves a second assignment behind and the operator gets to find
//     out which one the reader takes. `--write` replaces the line instead, so the documented setup is one
//     idempotent command.
import { randomBytes, scryptSync } from "node:crypto";
import { chmodSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// N=16384, r=8, p=1 is scrypt's "interactive" setting: ~16 MiB and ~100 ms per attempt, which is a
// negligible cost on one login and a punishing one on an offline guessing run. The parameters are stored IN
// the hash, so raising them later does not invalidate a hash produced today.
const N = 16384;
const r = 8;
const p = 1;
const KEY_LENGTH = 32;
const SALT_LENGTH = 16;

const chunks = [];
for await (const chunk of process.stdin) chunks.push(chunk);
let password = Buffer.concat(chunks).toString("utf8");

// One trailing newline is stripped, because `echo` adds one and a password nobody typed is a password
// nobody can retype. The stripping is announced rather than silent.
if (password.endsWith("\r\n")) password = password.slice(0, -2);
else if (password.endsWith("\n")) password = password.slice(0, -1);
if (chunks.length > 0 && Buffer.concat(chunks).toString("utf8") !== password) {
  process.stderr.write("note: a trailing newline was stripped from the password (use `printf %s` to avoid the ambiguity)\n");
}

if (password === "") {
  process.stderr.write("error: no password on stdin. Usage: printf %s 'your-password' | node scripts/hash-password.mjs\n");
  process.exit(2);
}
if (password.length < 16) {
  process.stderr.write(`note: ${password.length} characters. This is the only credential on the console's door and it is not rate-limited — longer is the whole defense.\n`);
}

const salt = randomBytes(SALT_LENGTH);
const key = scryptSync(password, salt, KEY_LENGTH, { N, r, p });
const line = `PALAI_CONSOLE_PASSWORD_HASH=scrypt.${N}.${r}.${p}.${salt.toString("base64url")}.${key.toString("base64url")}`;

if (!process.argv.includes("--write")) {
  process.stdout.write(`${line}\n`);
} else {
  // The console's OWN .env.local, resolved from this file rather than from the cwd. The docs invoke this
  // script from the repo root as often as from apps/web-console, and a cwd-relative path would put the
  // operator's password hash in whichever directory they happened to be standing in.
  const envFile = resolve(dirname(fileURLToPath(import.meta.url)), "..", ".env.local");

  let existing = "";
  try {
    existing = readFileSync(envFile, "utf8");
  } catch (err) {
    if (err.code !== "ENOENT") throw err;
  }

  // IDEMPOTENT, which is the whole point of writing the file rather than printing it: every prior assignment
  // of this variable is dropped — including one left by the old `>>` recipe, and including an `export `-
  // prefixed one — so a second run replaces the password instead of leaving two lines for the reader to
  // choose between. Every other line is carried through byte for byte; this file usually also holds
  // PALAI_API_KEY.
  const kept = existing.split("\n").filter((l) => !/^\s*(export\s+)?PALAI_CONSOLE_PASSWORD_HASH\s*=/.test(l));
  while (kept.length > 0 && kept[kept.length - 1].trim() === "") kept.pop();
  writeFileSync(envFile, `${[...kept, line].join("\n")}\n`, { mode: 0o600 });
  // Set the mode explicitly too: writeFileSync's `mode` applies only when it CREATES the file, so a
  // .env.local the operator already had would otherwise keep whatever their umask gave it.
  chmodSync(envFile, 0o600);
  // The path, never the value — this goes to stderr so nothing about `--write` prints a credential.
  process.stderr.write(`wrote PALAI_CONSOLE_PASSWORD_HASH to ${envFile} (mode 600)\n`);
}
