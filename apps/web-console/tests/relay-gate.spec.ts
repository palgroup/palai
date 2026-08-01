import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import { test, expect } from "@playwright/test";

// THE GATE COUNT (E25 T1, §2). The identity check lives INSIDE THE RELAY — not in a proxy.ts (§3.5
// N1/N2/N3, GHSA-f82v-jwr5-mffw at CVSS 9.1) and not in app/layout.tsx (§3.5 N5: Partial Rendering does
// not re-run a layout on every navigation). That decision is only worth as much as its enforcement, so
// this test COUNTS every exported HTTP method of every Route Handler under app/api and requires each one
// to open with the session gate. A fifth ungated export FAILS here.
//
// THREE THINGS MAKE IT UN-DECAYABLE, each answering a way this tree's guards have actually failed:
//
//  1. THE FILE SET IS WALKED, NOT LISTED. A guard that names two paths cannot see a third relay file
//     (`path-and-membership-comparisons-are-guilty`: every hardcoded path/membership comparison in this
//     tree's verifiers has shipped defeated). Discovery is a directory walk; a new route.ts is in scope
//     the moment it exists, and a walk that finds too little trips the floor below.
//  2. IT READS TOKENS, NOT TEXT. The tokens come from TypeScript's OWN scanner (`typescript/unstable/ast`
//     — already a devDependency, no new one is added), so a commented-out `// export async function PUT`
//     and a string holding the gate's spelling are BOTH invisible to it. A regex sweep over source text is
//     fooled by either, and this file's first draft was ALSO fooled by something subtler: a bare token
//     scan has no parser context, so `/^(?:text\/html|…)/i` in the relay tokenized as division and the
//     stray quote inside `/filename\*?=(?:UTF-8''|")?/i` opened a "string" that swallowed all four GET/
//     POST/PATCH/DELETE declarations. It reported 1 method of 5 and would have reported "0 ungated".
//     Regex literals are therefore re-scanned the way TypeScript's own parser does it, and:
//  3. THE SCAN PROVES IT DID NOT DESYNC. Brace/paren/bracket balance must come out at zero, and every
//     SyntaxKind member this file names must exist. Both are what a silent mis-scan breaks — and TS 7
//     renamed one of them already (`EndOfFileToken` → `EndOfFile`, while `formatSyntaxKind` still prints
//     the old name), which on the first run was an unterminated loop and a heap-exhaustion crash.
//
// The gate's shape is pinned exactly, as the first two statements of the body:
//
//     const refused = requireSession(request);
//     if (refused !== null) return refused;
//
// Anything else — a gate further down, a gate on a different value, a gate whose result is not returned —
// FAILS with the offending method named.
const HTTP_METHODS = new Set(["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"]);
const API_ROOT = resolve(process.cwd(), "app", "api");

// The relay serves six HTTP methods today (FIVE on the /v1 catch-all — GET/POST/PUT/PATCH/DELETE — and one
// on the stream), plus the login route's own POST: seven. PUT arrived with E29's desired-configuration
// write path, and its absence would have been invisible in exactly the way this file's header describes: a
// Route Handler serves only the methods it EXPORTS, so the form would have shipped against a 405.
//
// This FLOOR is not the check: every discovered export is checked whatever the count. It is the
// anti-vacuity guard, and it has already earned its place by catching a scan that found one method of five
// and was about to report zero ungated.
const KNOWN_HTTP_METHOD_FLOOR = 7;

function routeFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = resolve(dir, entry.name);
    if (entry.isDirectory()) out.push(...routeFiles(full));
    else if (entry.name === "route.ts" || entry.name === "route.tsx") out.push(full);
  }
  return out;
}

interface Token {
  kind: number;
  text: string;
}

// KINDS names every SyntaxKind member this file depends on. tokenize() throws if any is absent, so a
// rename in a TypeScript upgrade is a loud failure instead of a branch that quietly stops matching.
const KINDS = [
  "EndOfFile",
  "ExportKeyword",
  "AsyncKeyword",
  "FunctionKeyword",
  "Identifier",
  "OpenParenToken",
  "CloseParenToken",
  "OpenBraceToken",
  "CloseBraceToken",
  "OpenBracketToken",
  "CloseBracketToken",
  "LessThanToken",
  "ConstKeyword",
  "EqualsToken",
  "SemicolonToken",
  "IfKeyword",
  "ExclamationEqualsEqualsToken",
  "NullKeyword",
  "ReturnKeyword",
  "SlashToken",
  "SlashEqualsToken",
  "RegularExpressionLiteral",
  "TemplateHead",
  "TemplateMiddle",
  "TemplateTail",
  "StringLiteral",
  "NumericLiteral",
  "NoSubstitutionTemplateLiteral",
  "PlusPlusToken",
  "MinusMinusToken",
  "ThisKeyword",
  "SuperKeyword",
  "TrueKeyword",
  "FalseKeyword",
] as const;

type Kinds = Record<(typeof KINDS)[number], number>;

// endsExpression reports whether a token can be the END of an expression — the standard lexer test for
// telling `a / b` from `/regex/`, and the same question TypeScript's parser answers before calling
// reScanSlashToken. Everything else (an operator, `(`, `,`, `return`, `=`, `:`) means a `/` opens a regex.
function endsExpression(token: Token | undefined, K: Kinds): boolean {
  if (token === undefined) return false;
  return (
    token.kind === K.Identifier ||
    token.kind === K.StringLiteral ||
    token.kind === K.NumericLiteral ||
    token.kind === K.NoSubstitutionTemplateLiteral ||
    token.kind === K.RegularExpressionLiteral ||
    token.kind === K.CloseParenToken ||
    token.kind === K.CloseBracketToken ||
    token.kind === K.CloseBraceToken ||
    token.kind === K.PlusPlusToken ||
    token.kind === K.MinusMinusToken ||
    token.kind === K.ThisKeyword ||
    token.kind === K.SuperKeyword ||
    token.kind === K.TrueKeyword ||
    token.kind === K.FalseKeyword ||
    token.kind === K.NullKeyword
  );
}

async function tokenize(file: string): Promise<{ tokens: Token[]; K: Kinds }> {
  const ast = await import("typescript/unstable/ast");
  const raw = ast.SyntaxKind as unknown as Record<string, number | undefined>;
  const K = {} as Kinds;
  for (const name of KINDS) {
    const value = raw[name];
    if (typeof value !== "number") {
      throw new Error(`SyntaxKind.${name} does not exist in typescript ${(await import("typescript")).version} — this audit cannot be trusted until it is remapped`);
    }
    (K as Record<string, number>)[name] = value;
  }

  const scanner = ast.createScanner(true, ast.LanguageVariant.Standard, readFileSync(file, "utf8"));
  const tokens: Token[] = [];
  // TypeScript's scanner is context-free; its parser supplies the two contexts below by re-scanning. Both
  // are supplied here for the same reason: without them the stream desyncs on ORDINARY relay code, and a
  // desynced stream is a guard that reports what it never read.
  //
  //   brace/template stack — after a `${…}` substitution the `}` must be re-scanned as a template middle or
  //   tail. A raw scan reads it as CloseBraceToken and then lexes the rest of the template as source, which
  //   is how `enqueue(`${JSON.stringify(obj)}\n`)` in the stream relay unbalanced its braces.
  const stack: ("brace" | "template")[] = [];
  for (let i = 0; ; i++) {
    if (i > 200_000) throw new Error(`the token scan of ${file} never reached EndOfFile`);
    let kind = scanner.scan();
    if (kind === K.EndOfFile) break;
    //   regex context — a `/` that cannot be division is a regular-expression literal, re-scanned as ONE
    //   token so its contents (quotes, braces, escaped slashes) cannot be read as source.
    if ((kind === K.SlashToken || kind === K.SlashEqualsToken) && !endsExpression(tokens[tokens.length - 1], K)) {
      kind = scanner.reScanSlashToken();
    }
    if (kind === K.TemplateHead) stack.push("template");
    else if (kind === K.OpenBraceToken) stack.push("brace");
    else if (kind === K.CloseBraceToken) {
      if (stack[stack.length - 1] === "template") {
        kind = scanner.reScanTemplateToken(false);
        if (kind === K.TemplateTail) stack.pop();
      } else {
        stack.pop();
      }
    }
    tokens.push({ kind, text: scanner.getTokenText() });
  }

  // DESYNC CHECK. A swallowed string or an unrecognised regex leaves the bracket counts unbalanced, which
  // is exactly the failure that made this file report 1 method of 5.
  const pairs: [number, number, string][] = [
    [K.OpenBraceToken, K.CloseBraceToken, "{}"],
    [K.OpenParenToken, K.CloseParenToken, "()"],
    [K.OpenBracketToken, K.CloseBracketToken, "[]"],
  ];
  for (const [open, close, label] of pairs) {
    const depth = tokens.reduce((d, t) => d + (t.kind === open ? 1 : t.kind === close ? -1 : 0), 0);
    if (depth !== 0) throw new Error(`the token scan of ${file} desynced: ${label} depth ended at ${depth} — a string or regex literal was mis-scanned`);
  }
  return { tokens, K };
}

interface Handler {
  file: string;
  method: string;
  gated: boolean;
  detail: string;
}

// handlers finds every `export [async] function <HTTP_METHOD>(…)` and reports whether its body opens with
// the gate. `runtime`/`dynamic` config exports are not function declarations, so they are naturally out of
// scope — what is counted is the set of METHODS the framework will route to.
async function handlers(file: string): Promise<Handler[]> {
  const { tokens, K } = await tokenize(file);
  const found: Handler[] = [];
  for (let i = 0; i < tokens.length; i++) {
    if (tokens[i].kind !== K.ExportKeyword) continue;
    let j = i + 1;
    if (tokens[j]?.kind === K.AsyncKeyword) j++;
    if (tokens[j]?.kind !== K.FunctionKeyword) continue;
    const name = tokens[j + 1];
    if (name?.kind !== K.Identifier || !HTTP_METHODS.has(name.text)) continue;

    // Walk the parameter list to its closing paren, capturing the FIRST parameter's name — the gate has to
    // be handed the request THIS method received, not some other value in scope.
    let k = j + 2;
    if (tokens[k]?.kind !== K.OpenParenToken) continue;
    const firstParam = tokens[k + 1]?.kind === K.Identifier ? tokens[k + 1].text : "";
    let parens = 0;
    for (; k < tokens.length; k++) {
      if (tokens[k].kind === K.OpenParenToken) parens++;
      else if (tokens[k].kind === K.CloseParenToken && --parens === 0) break;
    }
    // Skip a return-type annotation. Generic arguments can hold braces (`Promise<{ ok: boolean }>`), so the
    // body brace is the first one at angle-bracket depth zero. `>`, `>>` and `>>>` all arrive as tokens whose
    // text is only `>` characters, which is what makes this counting exact rather than approximate.
    let angle = 0;
    let body = -1;
    for (let m = k + 1; m < tokens.length; m++) {
      if (tokens[m].kind === K.LessThanToken) angle++;
      else if (/^>+$/.test(tokens[m].text)) angle -= tokens[m].text.length;
      else if (tokens[m].kind === K.OpenBraceToken && angle <= 0) {
        body = m;
        break;
      }
    }
    if (body === -1) {
      found.push({ file, method: name.text, gated: false, detail: "no function body found" });
      continue;
    }

    // The pinned shape, token for token.
    const t = (n: number) => tokens[body + n];
    const id = t(2)?.text ?? "";
    const shaped =
      t(1)?.kind === K.ConstKeyword &&
      t(2)?.kind === K.Identifier &&
      t(3)?.kind === K.EqualsToken &&
      t(4)?.kind === K.Identifier &&
      t(4)?.text === "requireSession" &&
      t(5)?.kind === K.OpenParenToken &&
      t(6)?.kind === K.Identifier &&
      t(6)?.text === firstParam &&
      t(7)?.kind === K.CloseParenToken &&
      t(8)?.kind === K.SemicolonToken &&
      t(9)?.kind === K.IfKeyword &&
      t(10)?.kind === K.OpenParenToken &&
      t(11)?.text === id &&
      t(12)?.kind === K.ExclamationEqualsEqualsToken &&
      t(13)?.kind === K.NullKeyword &&
      t(14)?.kind === K.CloseParenToken &&
      t(15)?.kind === K.ReturnKeyword &&
      t(16)?.text === id &&
      t(17)?.kind === K.SemicolonToken;
    const opening = [1, 2, 3, 4, 5, 6, 7, 8].map((n) => t(n)?.text ?? "<eof>").join(" ");
    found.push({ file, method: name.text, gated: shaped, detail: shaped ? "gated" : `body opens with: ${opening}` });
  }
  return found;
}

test("every exported relay method passes through requireSession — an ungated export is a FAIL", async () => {
  const files = routeFiles(API_ROOT);
  expect(files.length, `no Route Handler files were discovered under ${API_ROOT}`).toBeGreaterThan(0);

  const all: Handler[] = [];
  for (const file of files) all.push(...(await handlers(file)));

  // eslint-disable-next-line no-console -- the count IS the evidence (AdminConsoleProof (a) recomputes it).
  console.log(
    `RELAY GATE AUDIT — ${files.length} route file(s), ${all.length} exported HTTP method(s), ${all.filter((h) => h.gated).length} gated:\n` +
      all.map((h) => `  ${h.gated ? "GATED  " : "UNGATED"} ${h.method.padEnd(6)} ${h.file.replace(process.cwd(), ".")}  ${h.detail}`).join("\n"),
  );

  expect(all.length, "fewer HTTP methods were found than the surface that exists — the walk or the scan is broken").toBeGreaterThanOrEqual(KNOWN_HTTP_METHOD_FLOOR);

  // /api/console/login is the ONE same-origin route that must NOT require a session — it is how a session
  // is obtained. It is named here, once, as the single exception, exactly as public-api-only.spec.ts names
  // it as the single non-/api/palai/ path. Everything else is gated.
  const LOGIN_ROUTE = resolve(API_ROOT, "console", "login", "route.ts");
  const gateable = all.filter((h) => h.file !== LOGIN_ROUTE);
  expect(all.length - gateable.length, "exactly one login method is exempt from the gate").toBe(1);

  const ungated = gateable.filter((h) => !h.gated);
  expect(ungated.map((h) => `${h.method} in ${h.file.replace(process.cwd(), ".")} (${h.detail})`), "relay methods that do NOT open with requireSession").toEqual([]);
  // The counted form of the same claim, stated as AdminConsoleProof (a) will state it.
  expect(gateable.filter((h) => h.gated).length, "gated method count must EQUAL exported method count").toBe(gateable.length);
});
