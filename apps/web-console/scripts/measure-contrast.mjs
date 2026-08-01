// EVERY CONTRAST NUMBER THIS CONSOLE PUBLISHES, RE-DERIVED FROM app/globals.css ITSELF.
//
//   node scripts/measure-contrast.mjs            every pair, both schemes
//   node scripts/measure-contrast.mjs --failing   only the pairs under their floor (exit 1 if any)
//
// WHY IT EXISTS. This tree's own rule is that a number in a comment must carry the command that produced it,
// because a number nobody can re-run is a number that goes stale in the safe-looking direction. The token
// file is full of measured pairs — "5.92 | 8.97", "3.69:1 light and 4.45:1 dark" — and until now every one of
// them was a value somebody computed once, somewhere else, and typed. Editing a single HSL step silently
// falsified all of them.
//
// WHAT IT IS NOT. It is not tests/contrast.spec.ts and does not replace it. That spec measures the RENDERED
// PAGE and catches the failure this script structurally cannot: "this control does not use the palette". This
// script measures the palette. Both are needed and the spec is the stronger of the two — which is exactly why
// this one parses the stylesheet rather than carrying its own copy of the colours.
//
// THE FORMULA IS tests/contrast.spec.ts's, character for character: WCAG 2.x relative luminance over
// sRGB-linearised channels, ratio (L1 + 0.05) / (L2 + 0.05), rounded to two places the way the spec's tables
// are. If the two ever disagree, one of them has been edited and the other has not.
//
// IT WAS WRITTEN FOR THE GREY SWAP. E30 replaced Radix Slate with Radix Sand to match the reference's warm
// grey, which moves the surface behind EVERY pair in this file at once — and the file carried a dozen typed
// ratios that would all have gone quietly stale together. That is the exact failure this repository's first
// planning rule names, so the numbers now carry the command that produces them.

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
// COMMENTS ARE STRIPPED FIRST, AND THIS IS NOT TIDINESS — it is the bug this script hit on its own first run.
// A declaration matcher whose value is `[^;]+` spans newlines, and app/globals.css's comments are prose that
// contains colons and no semicolons ("--border-control is separate from --border-hairline: one draws a
// region…"). That prose matched as a declaration and swallowed everything up to the next real `;`, which
// happened to be --danger-border-control — so the token vanished from the map while the file plainly
// declared it. A parser that reads comments as code is a parser whose output is one comment edit away from
// being wrong about something else.
const css = readFileSync(resolve(here, "../app/globals.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");

/**
 * Layer 1 is `--_name: H S% L%` and layer 2 is `--name: hsl(var(--_x))`, so the whole file resolves with two
 * passes and no CSS engine. Anything more complicated than an alias is deliberately NOT supported: a token
 * whose value this cannot resolve is a token this cannot judge, and it says so rather than guessing.
 */
function scopeOf(text) {
  const raw = new Map();
  for (const m of text.matchAll(/(--[A-Za-z0-9_-]+)\s*:\s*([^;]+);/g)) raw.set(m[1], m[2].trim());
  return raw;
}

// The :root block, then the dark override block. The dark block is found by its media query, which is also
// the ORDERING RULE app/globals.css warns about at the top — if a :root ever moves below it, the values this
// script reports for dark stop matching what a browser resolves, and the file's own comment says why.
const rootBlock = /:root\s*\{([\s\S]*?)\n\}/.exec(css);
const darkBlock = /@media\s*\(prefers-color-scheme:\s*dark\)\s*\{\s*:root\s*\{([\s\S]*?)\n\s*\}/.exec(css);
if (rootBlock === null || darkBlock === null) throw new Error("app/globals.css: could not find the :root block and its dark override");

const light = scopeOf(rootBlock[1]);
const dark = new Map([...light, ...scopeOf(darkBlock[1])]);

function hslToRgb(h, s, l) {
  const sn = s / 100;
  const ln = l / 100;
  const k = (n) => (n + h / 30) % 12;
  const a = sn * Math.min(ln, 1 - ln);
  const f = (n) => ln - a * Math.max(-1, Math.min(k(n) - 3, Math.min(9 - k(n), 1)));
  return [Math.round(f(0) * 255), Math.round(f(8) * 255), Math.round(f(4) * 255)];
}

/** resolve follows `hsl(var(--_x))` aliases down to a triple, or throws naming the token it could not read. */
function rgbOf(scope, token) {
  const value = scope.get(token);
  if (value === undefined) throw new Error(`${token} is not declared`);
  const alias = /^hsl\(\s*var\((--[A-Za-z0-9_-]+)\)\s*\)$/.exec(value);
  if (alias !== null) return rgbOf(scope, alias[1]);
  const parts = /^([\d.]+)\s+([\d.]+)%\s+([\d.]+)%$/.exec(value);
  if (parts === null) throw new Error(`${token} = "${value}" is not a raw HSL triple or a plain alias`);
  return hslToRgb(Number(parts[1]), Number(parts[2]), Number(parts[3]));
}

function relativeLuminance([r, g, b]) {
  const channel = (v) => {
    const s = v / 255;
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

function ratio(scope, a, b) {
  const la = relativeLuminance(rgbOf(scope, a));
  const lb = relativeLuminance(rgbOf(scope, b));
  const [hi, lo] = la >= lb ? [la, lb] : [lb, la];
  return Math.round(((hi + 0.05) / (lo + 0.05)) * 100) / 100;
}

// EVERY PAIR THIS CONSOLE PUBLISHES A NUMBER FOR. The floors are the criteria, named: 3 is SC 1.4.11
// Non-text Contrast (a control boundary, a focus ring, a mark); 4.5 is SC 1.4.3 Contrast (Minimum) for text
// under 18.66px, which is every word on this surface.
const SURFACES = [
  ["page", "--bg-page"],
  ["band", "--bg-subtle"],
  ["inset", "--bg-inset"],
  ["hovered row", "--bg-hover"],
  ["active row", "--bg-active"],
];

const pairs = [];
// THE GREY RAMP CARRIES THE WHOLE CONSOLE and it was just swapped from Radix Slate to Radix Sand to match the
// reference's warm grey, so every pair built on it is re-derived here rather than inherited from a comment.
for (const [name, token] of SURFACES) {
  pairs.push({ group: "text", what: `body text on ${name}`, fg: "--text", bg: token, floor: 4.5 });
  pairs.push({ group: "text", what: `muted text on ${name}`, fg: "--text-muted", bg: token, floor: 4.5 });
}
for (const [name, token] of SURFACES.slice(0, 3)) {
  pairs.push({ group: "control boundary", what: `control border on ${name}`, fg: "--border-control", bg: token, floor: 3 });
  pairs.push({ group: "control boundary", what: `hovered border on ${name}`, fg: "--border-control-hover", bg: token, floor: 3 });
  pairs.push({ group: "control boundary", what: `destructive border on ${name}`, fg: "--danger-border-control", bg: token, floor: 3 });
}
pairs.push({ group: "accent", what: "accent text on page", fg: "--accent-text", bg: "--bg-page", floor: 4.5 });
pairs.push({ group: "accent", what: "accent text on band", fg: "--accent-text", bg: "--bg-subtle", floor: 4.5 });
pairs.push({ group: "accent", what: "accent text on inset", fg: "--accent-text", bg: "--bg-inset", floor: 4.5 });
pairs.push({ group: "accent", what: "accent text on accent fill", fg: "--accent-text", bg: "--accent-bg", floor: 4.5 });
pairs.push({ group: "accent", what: "text on solid accent", fg: "--text-on-solid", bg: "--accent-solid", floor: 4.5 });
pairs.push({ group: "accent", what: "text on hovered solid", fg: "--text-on-solid", bg: "--accent-solid-hover", floor: 4.5 });
pairs.push({ group: "accent", what: "body text on accent fill", fg: "--text", bg: "--accent-bg", floor: 4.5 });
pairs.push({ group: "accent", what: "muted text on accent fill", fg: "--text-muted", bg: "--accent-bg", floor: 4.5 });
pairs.push({ group: "accent", what: "solid accent on page", fg: "--accent-solid", bg: "--bg-page", floor: 3 });
pairs.push({ group: "focus", what: "ring on page", fg: "--focus-ring", bg: "--bg-page", floor: 3 });
pairs.push({ group: "focus", what: "ring on band", fg: "--focus-ring", bg: "--bg-subtle", floor: 3 });
pairs.push({ group: "focus", what: "ring on hovered row", fg: "--focus-ring", bg: "--bg-hover", floor: 3 });

for (const tone of ["ok", "warn", "danger", "info", "live", "neutral"]) {
  pairs.push({ group: `status ${tone}`, what: "word on its fill", fg: `--${tone}-text`, bg: `--${tone}-bg`, floor: 4.5 });
  pairs.push({ group: `status ${tone}`, what: "body text on its fill", fg: "--text", bg: `--${tone}-bg`, floor: 4.5 });
  // The pill's BORDER against the page behind it. It is not a control boundary — a status pill is a <span>
  // with no handler and no role, so SC 1.4.11 does not judge it — but a band whose border vanishes into the
  // page is a band with no shape, and shape is one of the three carriers.
  pairs.push({ group: `status ${tone}`, what: "its border on the page", fg: `--${tone}-border`, bg: "--bg-page", floor: 1.3 });
}
pairs.push({ group: "status ok", what: "inline text on page", fg: "--ok-text-inline", bg: "--bg-page", floor: 4.5 });
pairs.push({ group: "status danger", what: "inline text on page", fg: "--danger-text-inline", bg: "--bg-page", floor: 4.5 });

const onlyFailing = process.argv.includes("--failing");
let failures = 0;
let group = "";
for (const pair of pairs) {
  const l = ratio(light, pair.fg, pair.bg);
  const d = ratio(dark, pair.fg, pair.bg);
  const bad = l < pair.floor || d < pair.floor;
  if (bad) failures += 1;
  if (onlyFailing && !bad) continue;
  if (pair.group !== group) {
    group = pair.group;
    console.log(`\n${group}`);
  }
  const flag = bad ? "  << UNDER " + String(pair.floor) : "";
  console.log(`  ${pair.what.padEnd(26)} light ${l.toFixed(2).padStart(6)}:1   dark ${d.toFixed(2).padStart(6)}:1   floor ${String(pair.floor)}${flag}`);
}
console.log(`\nPALETTE SWEEP — ${String(pairs.length)} pair(s) over 2 colour scheme(s), ${String(failures)} under floor`);
process.exit(failures === 0 ? 0 : 1);
