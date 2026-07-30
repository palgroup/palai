#!/usr/bin/env node
// Turn a console password into the value of PALAI_CONSOLE_PASSWORD_HASH. One file, zero dependencies, and
// the password is read from STDIN — never from argv, where it would be visible in `ps`, in a shell history
// file and in any process listing the machine keeps.
//
//   printf %s 'a-long-console-password' | node scripts/hash-password.mjs >> .env.local
//
// The output format is `scrypt$N$r$p$salt$key`, base64URL and unpadded so the whole line contains exactly
// one `=` (the env var's own) and needs no quoting. lib/session.ts only ever READS this format; this script
// is the only thing that writes it. They are deliberately not coupled by an import — this file has to stay
// something an operator can read end to end before trusting it with a password — so the binding between
// them is a test: tests/auth.spec.ts signs in with a password whose hash came out of this script.
import { randomBytes, scryptSync } from "node:crypto";

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
process.stdout.write(`PALAI_CONSOLE_PASSWORD_HASH=scrypt$${N}$${r}$${p}$${salt.toString("base64url")}$${key.toString("base64url")}\n`);
