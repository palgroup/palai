import "server-only";

import { createHmac, scrypt as scryptCallback, timingSafeEqual } from "node:crypto";
import { promisify } from "node:util";

import { problem } from "@/lib/relay";

// THE CONSOLE'S DOOR (E25 T1). One operator, one password, one signed cookie — and NO new dependency: the
// whole thing is node:crypto's scrypt (a memory-hard KDF, so a stolen hash is expensive to attack) plus an
// HMAC, which is why there is no bcrypt/argon2/iron-session/next-auth in package.json.
//
// WHAT THIS IS NOT, stated at the top because it bounds every claim below: this is a DOOR, not an identity
// system. There are no users, no roles, no audit trail and no session revocation list. A principal on the
// server side is still `key:<api_key_id>` (known-gaps HIL-P2) — every action taken through this console is
// recorded against the console's KEY, never against a person. And the key behind the door is still
// UNLIMITED: `Scope.HasScope` (api/middleware/auth.go:31-34) returns true for every capability when the
// scope set is empty, which is exactly the bootstrap key the README tells the operator to export. Closing
// the door does not narrow the key; docs/operations/console.md writes the narrow-key recipe, and measuring
// which pages actually work with it is §6 operator leg 4.
//
// THREE MECHANISMS, each with its reason:
//
//  1. FAIL-CLOSED. No PALAI_CONSOLE_PASSWORD_HASH, no service — the same posture lib/palai.ts:30-39 already
//     takes for PALAI_API_KEY. A misconfiguration's silent state cannot be OPEN, because the thing behind
//     this origin is a write proxy holding an unlimited credential.
//  2. A SIGNED, STATELESS COOKIE. Contents are an expiry and a signature over it; there is no server-side
//     session table, so nothing to grow, migrate or leak. The signing key is DERIVED from the password hash,
//     which has a consequence worth naming: changing the password invalidates every live session. That is
//     the only revocation this design has, and it is deliberate rather than accidental.
//  3. CSRF IN TWO LAYERS. SameSite=Strict (a DELIBERATE divergence from the vendor's own `lax` example,
//     §3.5 N7 — the cost is that a link from another site lands on a console that looks signed out, and the
//     operator clicks once more) AND a manual Origin comparison, because a Route Handler gets NO framework
//     CSRF protection the way a Server Action does (§3.5 N8) and this console deliberately uses no Server
//     Actions (a second write path the public-API-only proof could not see).
export const SESSION_COOKIE = "palai_console_session";

// Twelve hours: long enough for a working day, short enough that a forgotten browser is not a standing
// grant. There is no refresh — an expired session means logging in again.
const SESSION_TTL_MS = 12 * 60 * 60 * 1000;

const scrypt = promisify(scryptCallback) as (password: string, salt: Buffer, keylen: number, options: { N: number; r: number; p: number }) => Promise<Buffer>;

// A stored hash is `scrypt$N$r$p$salt$key`, base64URL and unpadded so the whole value contains no `=` and
// drops straight into an env file or a `NAME=value` line without quoting. scripts/hash-password.mjs is the
// only thing that WRITES this format; this module only ever reads it. They are bound by a test rather than
// by a shared import, because that script must stay a zero-dependency single file an operator can read.
interface StoredHash {
  N: number;
  r: number;
  p: number;
  salt: Buffer;
  key: Buffer;
}

function parseHash(value: string): StoredHash | null {
  const parts = value.split("$");
  if (parts.length !== 6 || parts[0] !== "scrypt") return null;
  const [N, r, p] = [Number(parts[1]), Number(parts[2]), Number(parts[3])];
  if (!Number.isInteger(N) || !Number.isInteger(r) || !Number.isInteger(p) || N < 2 || r < 1 || p < 1) return null;
  const salt = Buffer.from(parts[4], "base64url");
  const key = Buffer.from(parts[5], "base64url");
  if (salt.length < 16 || key.length < 16) return null;
  return { N, r, p, salt, key };
}

function storedHash(): { raw: string; parsed: StoredHash } | null {
  const raw = process.env.PALAI_CONSOLE_PASSWORD_HASH?.trim();
  if (!raw) return null;
  const parsed = parseHash(raw);
  return parsed === null ? null : { raw, parsed };
}

// signingKey derives the cookie's HMAC key from the password hash. The hash is already a high-entropy
// server-side secret, so this needs no second env var to configure and no key file to lose — and the
// coupling is a feature (see mechanism 2 above). The label pins the derivation to this one use.
function signingKey(rawHash: string): Buffer {
  return createHmac("sha256", rawHash).update("palai-console-session-v1").digest();
}

function sign(rawHash: string, expiresAt: number): string {
  return createHmac("sha256", signingKey(rawHash)).update(`v1.${expiresAt}`).digest("base64url");
}

// verifyPassword derives the key from the SUPPLIED password with the STORED parameters and compares in
// constant time. It runs the full derivation for every input — an absent, empty or malformed password
// included — so the time taken never reveals how far the check got, and the caller has exactly one thing to
// say back: the password was not accepted.
export async function verifyPassword(password: string): Promise<boolean> {
  const stored = storedHash();
  if (stored === null) return false;
  const { N, r, p, salt, key } = stored.parsed;
  const derived = await scrypt(password, salt, key.length, { N, r, p });
  return derived.length === key.length && timingSafeEqual(derived, key);
}

export function sessionCookie(): string {
  const stored = storedHash();
  if (stored === null) throw new Error("PALAI_CONSOLE_PASSWORD_HASH is absent — no session can be issued");
  const expiresAt = Date.now() + SESSION_TTL_MS;
  const value = `${expiresAt}.${sign(stored.raw, expiresAt)}`;
  // Secure is unconditional. The cost is named rather than worked around: a console served over plain HTTP
  // on a LAN address will not keep this cookie. `localhost` and `127.0.0.1` are potentially-trustworthy
  // origins in every current browser, so `pnpm dev` and the loopback proofs work; anything else is a TLS
  // edge (deploy/compose/production.yml already has one).
  return `${SESSION_COOKIE}=${value}; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age=${Math.floor(SESSION_TTL_MS / 1000)}`;
}

export function clearedSessionCookie(): string {
  return `${SESSION_COOKIE}=; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age=0`;
}

// readCookie parses one cookie out of the request header. Route Handlers get the raw header, and reading it
// directly keeps this module free of framework request types — the AST gate audit reads plain functions.
function readCookie(request: Request, name: string): string | null {
  const header = request.headers.get("cookie");
  if (header === null) return null;
  for (const pair of header.split(";")) {
    const eq = pair.indexOf("=");
    if (eq === -1) continue;
    if (pair.slice(0, eq).trim() === name) return pair.slice(eq + 1).trim();
  }
  return null;
}

function validSession(request: Request, rawHash: string): boolean {
  const value = readCookie(request, SESSION_COOKIE);
  if (value === null) return false;
  const dot = value.indexOf(".");
  if (dot === -1) return false;
  const expiresAt = Number(value.slice(0, dot));
  if (!Number.isFinite(expiresAt) || expiresAt <= Date.now()) return false;
  const presented = Buffer.from(value.slice(dot + 1), "base64url");
  const expected = Buffer.from(sign(rawHash, expiresAt), "base64url");
  return presented.length === expected.length && timingSafeEqual(presented, expected);
}

// sameOrigin is the CSRF second layer (§3.5 N8): three lines comparing the Origin the browser stamped on a
// mutating request against the Host it was sent to. A browser cannot forge Origin, so a form or fetch on
// another site cannot drive this relay even if SameSite were ever relaxed or bypassed. A mutating request
// with NO Origin is refused too — every browser sends it on a non-GET fetch, so its absence means the caller
// is not the console.
function sameOrigin(request: Request): boolean {
  if (request.method === "GET" || request.method === "HEAD") return true;
  const origin = request.headers.get("origin");
  const host = request.headers.get("host");
  if (origin === null || host === null) return false;
  try {
    return new URL(origin).host === host;
  } catch {
    return false;
  }
}

// requireSession is THE GATE, and it lives inside the relay: every exported method of every Route Handler
// under app/api/palai begins with it, and tests/relay-gate.spec.ts counts them to prove it. It returns the
// refusal to send, or null to proceed.
//
// The order of the three refusals is deliberate. Misconfiguration first (503), because a console with no
// password is not "logged out", it is not serving. Then the session (401), so an anonymous caller gets one
// uniform answer for every method and path. Origin last (403), because CSRF only matters to a caller who
// already has a session — checking it first would answer 403 to strangers and tell them less.
export function requireSession(request: Request): Response | null {
  const stored = storedHash();
  if (stored === null) {
    return problem(
      503,
      "console_not_configured",
      "PALAI_CONSOLE_PASSWORD_HASH is not set (or is not a valid scrypt hash), so this console serves nothing. " +
        "Generate one with `node scripts/hash-password.mjs` reading the password from stdin — see docs/operations/console.md.",
    );
  }
  if (!validSession(request, stored.raw)) {
    return problem(401, "authentication_required", "this console requires an operator session — sign in at /login");
  }
  if (!sameOrigin(request)) {
    return problem(403, "origin_mismatch", "a write must come from the console's own origin");
  }
  return null;
}
