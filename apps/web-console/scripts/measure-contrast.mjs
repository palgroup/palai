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

const LANES = ["lifecycle", "message", "model_step", "tool", "approval", "recovery", "usage", "outcome"];

// THE FLOORS ARE THE CRITERIA, NAMED. 3 is SC 1.4.11 Non-text Contrast (a mark, a rule, a control boundary);
// 4.5 is SC 1.4.3 Contrast (Minimum) for text under 18.66px, which is every word in this console.
const pairs = [];
for (const lane of LANES) {
  // The MARK against all three surfaces a strip can sit on. `--bg-inset` is the code/panel band and the one a
  // detail pane puts a strip on; `--bg-hover` is a table row under the pointer, which is where the sessions
  // list's activity bar spends its time.
  pairs.push({ group: `lane ${lane}`, what: "mark on page", fg: `--lane-${lane}`, bg: "--bg-page", floor: 3 });
  pairs.push({ group: `lane ${lane}`, what: "mark on band", fg: `--lane-${lane}`, bg: "--bg-subtle", floor: 3 });
  pairs.push({ group: `lane ${lane}`, what: "mark on inset", fg: `--lane-${lane}`, bg: "--bg-inset", floor: 3 });
  pairs.push({ group: `lane ${lane}`, what: "mark on hovered row", fg: `--lane-${lane}`, bg: "--bg-hover", floor: 3 });
  // The BADGE: its word on its own fill, and its own fill against the page behind it.
  pairs.push({ group: `lane ${lane}`, what: "badge word on badge fill", fg: `--lane-${lane}-text`, bg: `--lane-${lane}-bg`, floor: 4.5 });
  pairs.push({ group: `lane ${lane}`, what: "badge rule on badge fill", fg: `--lane-${lane}`, bg: `--lane-${lane}-bg`, floor: 3 });
}
pairs.push({ group: "failure", what: "mark on page", fg: "--lane-failure", bg: "--bg-page", floor: 3 });
pairs.push({ group: "failure", what: "mark on inset", fg: "--lane-failure", bg: "--bg-inset", floor: 3 });
pairs.push({ group: "failure", what: "mark on hovered row", fg: "--lane-failure", bg: "--bg-hover", floor: 3 });

pairs.push({ group: "ink", what: "solid fill on page", fg: "--ink", bg: "--bg-page", floor: 3 });
pairs.push({ group: "ink", what: "text on solid fill", fg: "--text-on-solid", bg: "--ink", floor: 4.5 });
pairs.push({ group: "ink", what: "hover fill on page", fg: "--ink-hover", bg: "--bg-page", floor: 3 });
pairs.push({ group: "ink", what: "text on hover fill", fg: "--text-on-solid", bg: "--ink-hover", floor: 4.5 });
pairs.push({ group: "ink", what: "focus ring on page", fg: "--focus-ring", bg: "--bg-page", floor: 3 });
pairs.push({ group: "ink", what: "focus ring on hovered row", fg: "--focus-ring", bg: "--bg-hover", floor: 3 });
pairs.push({ group: "ink", what: "focus ring on band", fg: "--focus-ring", bg: "--bg-subtle", floor: 3 });

pairs.push({ group: "status live", what: "word on fill", fg: "--live-text", bg: "--live-bg", floor: 4.5 });
pairs.push({ group: "status ok", what: "word on fill", fg: "--ok-text", bg: "--ok-bg", floor: 4.5 });
pairs.push({ group: "status warn", what: "word on fill", fg: "--warn-text", bg: "--warn-bg", floor: 4.5 });
pairs.push({ group: "status danger", what: "word on fill", fg: "--danger-text", bg: "--danger-bg", floor: 4.5 });
pairs.push({ group: "status info", what: "word on fill", fg: "--info-text", bg: "--info-bg", floor: 4.5 });
pairs.push({ group: "status neutral", what: "word on fill", fg: "--neutral-text", bg: "--neutral-bg", floor: 4.5 });

pairs.push({ group: "chrome", what: "body text on page", fg: "--text", bg: "--bg-page", floor: 4.5 });
pairs.push({ group: "chrome", what: "muted text on page", fg: "--text-muted", bg: "--bg-page", floor: 4.5 });
pairs.push({ group: "chrome", what: "muted text on band", fg: "--text-muted", bg: "--bg-subtle", floor: 4.5 });
pairs.push({ group: "chrome", what: "muted text on inset", fg: "--text-muted", bg: "--bg-inset", floor: 4.5 });
pairs.push({ group: "chrome", what: "muted text on active row", fg: "--text-muted", bg: "--bg-active", floor: 4.5 });
pairs.push({ group: "chrome", what: "control border on page", fg: "--border-control", bg: "--bg-page", floor: 3 });
pairs.push({ group: "chrome", what: "control border on band", fg: "--border-control", bg: "--bg-subtle", floor: 3 });
pairs.push({ group: "chrome", what: "control border on inset", fg: "--border-control", bg: "--bg-inset", floor: 3 });
pairs.push({ group: "chrome", what: "destructive border on page", fg: "--danger-border-control", bg: "--bg-page", floor: 3 });

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
