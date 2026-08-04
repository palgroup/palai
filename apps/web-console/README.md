# @palai/web-console — open-core console (E17 T10)

A Next.js admin + live-run console over the Palai **public API only** (§47.6). The API key lives solely in
the server-side relay (`app/api/palai/**`) and never reaches the browser — the same server-relay stance as
`examples/nextjs-sdk` (the E13/E16 decision).

## Surfaces

- **Admin (§47.1, `/`):** projects, API keys, model connections/routes, secret-ref
  METADATA (the value is never shown), agent revisions + diff, knowledge bases. Every panel fetches a
  `/v1` list through the same-origin relay.
- **Live (§47.2, `/runs`):** start a run and watch its canonical event timeline sorted into lanes
  (messages / model steps / tool+subagent / approvals / usage / recovery+attempt / terminal). The
  **exact approval UI** shows the authoritative **operation / branch / request_hash** — the detail the
  canonical `approval.requested.v1` event actually carries (`packages/coordinator/publication.go`) — and
  renders the proposal-supplied `display` string in a separate, explicitly non-authoritative region that
  never replaces it. Recovery/attempt transitions and artifact download included.

- **Approvals (`/approvals`, E25 T5):** the queue of **gated tool calls** parked by a run —
  `GET /v1/approvals` plus the two decision routes E23 T9 opened, so a deployment with no Slack workspace can
  answer them at all. The screen is the server's (`slack.DeriveApprovalDisplay`, the same derivation the Slack
  message renders), the decision carries the row's own `request_hash`, and a deny takes a reason that reaches the
  model verbatim. **It holds tool approvals only** — publication approvals live in a run's stream, and the page
  says so. Operator guide: **[docs/operations/console.md](../../docs/operations/console.md) §4**.

- **Repositories (`/repositories`, E25 T6):** register the external repository a coding run attaches its
  workspace through (`POST /v1/repository-bindings`), and read the bindings back. `connection_ref` is a
  **handle** — a `secret_refs` NAME chosen from `GET /v1/secret-refs`, never a typed value and never a
  credential. Two ceilings are on the screen because an operator would otherwise assume otherwise:
  **registering a binding does not prove the repository is reachable** (nothing is cloned here; the first thing
  that exercises a binding is a run), and **there is no PATCH and no DELETE** — this console creates and reads,
  it does not correct.
- **Agents (`/agents`, E25 T6):** an agent is a **name with a lineage of immutable revisions**. Create the
  lineage, create a draft revision — with the **environment picker**, which is what binds T3's pipe so the
  agent's shell commands receive that environment's `KEY=value` pairs — then **publish**, which cannot be
  undone. A run is pinned to a published revision from `/runs`, where the agent and revision pickers are
  **optional**: with none chosen the stream relay's body is unchanged, which is why the E17 T10 journey specs
  needed no edit. A **draft** is listed and labelled rather than hidden, because the server refuses to run one
  (409 `revision_not_published`) and an operator who cannot see their draft cannot tell why. `tool_sets` (the
  GRANT) and `mcp_connections` (the CEILING) are **pickers as of E25 T7**, published sets only, and both read
  back on the revision list — the grant half was write-only until then, so a revision could name a set nobody
  could confirm.

- **Tools (`/tools`, E25 T7):** register an upstream **MCP connection** (`secret_ref` chosen from a list, never
  typed), **discover** its tools, read each DRAFT revision **with the description the server wrote**, approve it
  by publishing — carrying the `approval_required` gate and the operator's label on the same call — then pin the
  approved revisions into a set, publish the set and read its **contents** back. Two of those calls are routes
  E25 T7 added (`GET /v1/tools/{tool_id}/revisions`, `GET /v1/tool-sets/{set}/revisions/{revision_id}`); a
  SHIPPED runbook had told operators to publish a `$REV_ID` no route returned. **This is a registration screen,
  not the approval queue** — it shows the server's description because that is what is being decided, while
  `/approvals` does not carry that field at all. The description is rendered as **text** (proven by attack, not
  by claim), and the gate **defaults ON for every tool** because Palai does not classify write tools and will
  not start (`HIL-P5` is made visible, not closed). Operator guide:
  **[docs/operations/console.md](../../docs/operations/console.md) §4b**.

### Public-API GAP: richer approval detail

The `approval.requested.v1` event carries only `{publication_id, operation, branch, request_hash, display}`,
and there is **no `/v1/publications` or approvals READ endpoint** — so the event payload is all a real client
receives. Richer per-approval detail the exact-approval UI would ideally show — `action`, `args`, `diff`,
`destination`, `risk`, `expiry` — is **not on the public API today**. This is a named public-API GAP (the
same shape as the E13-T10 modelRoutes write-only gap), E18/hardening input, **not a console defect**: the
console renders those fields the moment the event (or a publications read endpoint) provides them. Until then
it honestly shows only what the API emits, and the proposal `display` string never stands in for it.

## The door (E25 T1)

The console asks for an operator password, and **does not serve without one**. One operator, one password, one
signed cookie; the identity check lives inside each relay method, not in a `middleware.ts`/`proxy.ts` and not
in a layout. Full operator guide: **[docs/operations/console.md](../../docs/operations/console.md)**.

```sh
printf %s 'a-long-console-password' | node scripts/hash-password.mjs --write
```

`--write` puts the line in `.env.local` and **replaces** any `PALAI_CONSOLE_PASSWORD_HASH=` already there, so
the step is idempotent. Without the flag it prints the line instead. The format is `scrypt.N.r.p.salt.key`,
dot-separated because `$` is expanded by every dotenv reader and the old `$` form did not survive `.env.local`
at all — `docs/operations/console.md` §2 has the measurement, `tests/env-file.spec.ts` has the proof, and a
`$` hash generated earlier is still accepted.

Then start with `PALAI_CONSOLE_PASSWORD_HASH` set, and open **`/login`** first — an unauthenticated visitor is
not redirected, they get `401` from every panel.

**Read the ceilings before relying on this.** It is a single-operator door, not an identity system: no users,
no roles, no audit trail (every approval is recorded against the console's *key*, `HIL-P2`), no session
revocation list, no sign-in rate limit, and the hash sits in an env var rather than a KMS. And **the key
behind the door is still unlimited** unless you narrow it — `Scope.HasScope`
(`apps/control-plane/api/middleware/auth.go:31-34`) grants every capability to an empty scope set, which is
exactly the bootstrap key the real-profile recipe below exports.

## Public-API-only relay

The browser talks ONLY to `/api/palai/*`, with **one** exception — `POST /api/console/login`, which is how a
session is obtained and which carries no Bearer. `tests/public-api-only.spec.ts` counts that exception in both
directions: it must occur, and nothing else may join it.

- `app/api/palai/v1/[...path]/route.ts` — the one generic data relay. It reconstructs the upstream path
  from the browser URL, so the ONLY thing the browser can address is a `/v1/*` public-API route; it
  forwards through `@palai/sdk` (server-side Bearer) and streams artifact bytes.
- `app/api/palai/stream/route.ts` — starts a run and re-projects the canonical SSE event stream to the
  browser as ndjson (lane-tagged), staying open across an approval pause.

## The token system: two layers, and layer 1 is HSL components (E29)

`app/globals.css` layer 1 is the raw scale, private behind `--_`, stored as `H S% L%` with **no `hsl()`
wrapper**; layer 2 is the semantic roles built on it, and rule bodies use layer 2 only. Storing components
rather than colours is what makes `hsl(var(--_slate-12) / 45%)` possible, so an opacity needs no token of its
own — the architecture is ported from `design-reference/design-system-measured.md` §1.

**The conversion is lossless and that is enforced rather than hoped.** Every one of the 60 raw steps
round-trips to the identical rgb triple at four decimal places, and `tests/contrast.spec.ts` re-measures the
pairs on the rendered page: **259 controls on 18 routes, 0 below 3:1, weakest 3.69:1 light / 4.45:1 dark** —
the same numbers as before the port, down to the rgb triples the skip-link test prints.

**What the port did NOT take, and why:** the reference's **grey ramp**. §1 names 29 steps and the measurement
captured four values; adopting it would mean inventing twenty-five and re-measuring every pair against a ramp
with no published contrast guarantees, where Radix's come with theirs. Its `rgba(255,255,255,0.10)` border is
separately refused — 1.30:1 against SC 1.4.11's 3:1, and `contrast.spec.ts` throws on a translucent value by
design. The **brand slot stays Palai's own** iris accent, built the same way. E30 took the rest of the
reference's system; see the next section.

**What it did take:** the two-layer architecture, weights `400/500/580/600` (580 is a variable-font
intermediate), a three-step **two-layer** shadow scale (near defines the edge, far defines height) with the
elevations separated — panel `sm`, popup `md`, modal `lg` — and **one** easing curve,
`cubic-bezier(.165,.84,.44,1)`, where this file had three and one of them was referenced by nothing.

**The token count did not drop, and that is the honest result: 138 → 141 names.** The reference's saving comes
from collapsing per-opacity tokens, and our variants are Radix *steps* rather than opacities of one colour, so
nothing collapsed there. What did become derived rather than declared: **eight hard-coded `rgba` shadow
literals across the two schemes became two alphas** (the dark block now changes two numbers instead of
re-stating two multi-layer values), three easing curves became one, and the dialog scrim — the last
hard-coded colour in any rule body — is composed off layer 1.

## Matching the reference: the grey, the row, the sectioned form (E30)

E29 took the reference's type scale, radii, shadows and easing. E30 takes the three things it left: the
**colour of the grey**, the **row metrics tree-wide**, and the **layout of a configuration screen**.

**The grey is warm now, and that is the single most visible change.** `design-system-measured.md` §1 measured
the reference's grey as warm — yellowish at the light end, `rgb(26,26,25)` for the body — and ours was Radix
**Slate**, hue 210–240, a *blue* grey. On a full screen no amount of type or spacing work compensates for
that. The ramp is now Radix **Sand** (hue 45–60): same twelve-step architecture, same role mapping, same
published contrast guarantees, one scale swapped. The dark ladder starts at **step 2** rather than step 1
because that is where the measurement lands — sand-2 is `#191918` against the reference's `#1a1a19`, one
8-bit level per channel — and `--bg-subtle` and `--bg-inset` resolve to the same value there, which is the
reference's own architecture (it has one surface token, `rgba(255,255,255,0.05)`, doing both jobs).

`node scripts/measure-contrast.mjs` re-derives **43 pairs over both schemes, 0 under floor**, from
`app/globals.css` itself — the swap moved the surface behind every published ratio in the file at once, and
a dozen typed numbers would otherwise have gone stale together.

**Row 46px, header 32px, tree-wide.** E29 scoped those to the two session screens with a note saying the
tree-wide pass owned them; this is that pass, so they are on `tbody td` / `thead th` and every table has them.
`height` on a cell is a minimum, so a cell holding a rename control still grows — and a cell holding a block
goes back to top alignment, because a multi-line cell centred against a single-line neighbour reads as
misaligned rather than centred.

**The two-column section, and it is a primitive rather than markup on one page.** Measured off the reference's
agent-detail page, 2026-08-01:

```
row          display:flex  gap:28px  width:768px
left  224px  the section title (15/20 w580) + its ONE-SENTENCE description
right 516px  the fields (control height 32px, filling the column)
```

`components/ResourceForm.tsx` renders **every one of its twelve callers** into that shape without any of them
being edited: `title` and `note` — props it has always taken — become the left column and `fields` become the
right. A `sections` prop handles a record whose configuration genuinely has several, which is the reference's
agent page exactly. **The 768px is the point, not the two columns**: our forms stretched a password field
across a 78rem page, and a measure this narrow is why the reference's detail pages read as documents somebody
fills in.

**The detail-page anatomy** (`app/agents/[id]`): breadcrumb carrying the **name** (the reference's agent trail
reads `Agents / palcore Mac Spike`; its *session* trail carries the id, because a session usually has no name
a person chose), the title with its status pill **inline**, then a quiet metadata line — the id with its copy
button and the lineage's facts, in the secondary colour with **no boxes**. That line replaced four bordered
chips: a chip is for a value you can act on, and a reading in a box is a box.

**Weight 580 is measured, not asserted.** The reference's scale ships `400/500/580/600` and 580 is the tell
that it uses a variable font. `getComputedStyle` reports the *specified* weight, so it says "580" on a machine
that drew 600 — `tests/contrast.spec.ts` lays the same string out at all four weights and compares rendered
widths instead. On this stack: `TYPE WEIGHT 580 — 580 renders as its own weight. 400=244.75px 500=250.91px
580=255.80px 600=257.02px`. It is a report and not a floor; a platform without a variable system font is not
a defect this console can fix, and the honest form of "we picked the nearest" is a line in the log.

**What was NOT taken, and why.** `rgba(255,255,255,0.10)` borders and `0.05` surfaces — 1.30:1 against SC
1.4.11's 3:1, and `contrast.spec.ts` throws on a translucent colour by design. What is matched instead is the
**result**: composite the reference's 0.05 white over its own body and you get `#232322`, which is sand step 3
— so our opaque surface *is* their surface, measurable where theirs is not. The difference that remains is
that theirs also draws CONTROL boundaries at 0.10 white, and ours draws them at step 10, which is the pair
`contrast.spec.ts` measures at 3.80:1 light / 4.14:1 dark.

**Two things the owner named.** `— no revision pinned` repeated down fifteen rows is now the em dash this
console uses for every absent value, with the explanation on `title` and stated **once with its count** in
`Panel`'s new `footnote` — the reference leaves an empty cell quiet. And the `SCOPE` box, four unlabelled
lines in a card at the top of the rail, is an unframed readout at its **foot** with each row's role in a
gutter; its "empty dropdown" was never styling — `String(row.display_name ?? row.id)` keeps a `display_name`
of `""`, because `""` is neither null nor undefined, so the fallback could not fire.

### The sixth status band, and the measurement that picked its hue (E31)

`active`, `running`, `provisioning` and `queued` matched nothing in `components/Status.tsx`'s classifier and
fell through to `neutral` — so on a list where nineteen of twenty sessions are active, the one column that
says what a session is *doing* was twenty identical grey pills. `revoked` (→ danger) and `pause`/`hold`
(→ warn, `‖`) were the two halves of the same defect that needed no new token; this is the third.

**The hue could not be taken from the reference, and that is a measurement rather than a shrug.**
`design-reference/` records exactly four chromatic families — `--_violet-450` (247.6°), `--_red-450` (0°),
`--_green-450` (120°), `--_brand-clay` (14.8°) — and the only colour it records for a pill at all is its
*surface*, `rgba(255,255,255,0.05)`, which is a **neutral**. The reference gives a running state no hue, so
there is no value to copy; and none of its four families is unclaimed, since violet is the accent slot (ours
is iris, and a status band wearing the link and focus colour would make a running session look like a
control), red is `danger`, green is `ok`, and clay is the brand colour the brief says to substitute ours for.

So the hue is ours, chosen on one criterion — distance from the bands it shares a column with. Taken:
red 0° · amber 46° · grass 131° · blue 206° · iris 240°. **Teal at 172°** sits 41° from grass and 34° from
blue, the two readings a running pill is next to: `ok` ("this finished") and `info` ("this is waiting").
Cyan was the obvious pick and is rejected for exactly that reason — at 190° it is 16° from `info`'s blue,
and *waiting* against *running* is the one adjacency this column cannot afford.

| | word on its fill (L \| D) | body text on its fill | border on page |
|---|---|---|---|
| `live` (teal) | 10.85 \| 11.42 | 14.67 \| 12.68 | 1.48 \| 2.10 |

`▸` is U+25B8, which carries no Unicode emoji property, so it needs no variation selector — colour is the
third carrier here exactly as it is for the other five bands. `node scripts/measure-contrast.mjs` →
**51 pairs, 0 under floor**.

### `caveat`: the note's background half, at full measure

The 224px column made a real problem visible rather than causing one. The reference's rule is one sentence
per section; several of ours were three — but three sentences of operator-critical fact ("create and rotate
are the same operation", "a field you cannot see is a field you did not send"). Cutting them to fit trades a
true safety statement for a layout. `ResourceForm` now takes a `caveat`: the note keeps its first sentence in
the column, the rest renders full-width under the rows in the `details.notes` shape this stylesheet already
argues for. Applied to `/environments`, `/registry` and `/policy` — the three longest.

### One prop off `/fleet`

Five `Admit` buttons in one table each carried `variant="primary"`, on a screen that already has two real
primaries in its heads. Seven filled controls on one page is a page with no primary action at all. Same word,
same testid, same handler — the default button's boundary is the `--border-control` this suite measures at
3.80:1 light / 4.14:1 dark.

## The primitive layer (`components/ui/`, E29)

Everything under `components/` was a DOMAIN component until this landed — `AgentDiff`, `ApprovalPanel`,
`Timeline` — and nothing under it was a control. Measured on `d8ca934b` (2026-07-31):

| | |
|---|---|
| `grep -rn '<select' --include='*.tsx' components app \| wc -l` | 7, every one native |
| `grep -rn 'role="menu"\|role="listbox"\|role="dialog"' … \| wc -l` | 0 |
| `grep -rn '<button' --include='*.tsx' components app \| wc -l` | 39 (38 elements + one in a comment) |

Seven primitives now sit under `components/ui/`, built on **[@base-ui/react](https://base-ui.com) 1.6.0** —
the MUI team's headless library, which supplies roles, keyboard behaviour, dismissal and Floating UI
positioning and **ships no styles at all**, so every pixel comes from `app/globals.css`'s tokens:

| primitive | what consumes it |
|---|---|
| `Button` | 16 files, every button in the console |
| `Select` | `Picker`, `Panel`'s sort, `Chrome`'s scope, the three session filters, the transcript's type filter |
| `Menu` | the session row's `⋯` |
| `Dialog` | `ConfirmDestructive` |
| `Badge` | `Status` |
| `Tabs` | the session transcript's Transcript/Debug strip |
| `Field` | `ResourceForm`, `Picker` |

**Import per component (`@base-ui/react/select`), never the barrel** — the package is tree-shakeable and the
barrel is not. The cost is a real number: the built client JS went from **897,670 → 1,135,499 bytes raw**
and **274,741 → 358,458 gzipped** (+30.5%), measured with

```
find .next/static -type f -name '*.js' -print0 | xargs -0 stat -f%z | awk '{s+=$1} END {print s}'
find .next/static -type f -name '*.js' -print0 | xargs -0 -n1 gzip -c | wc -c
```

Three of the library's behaviours break assertions this suite already ships, and each is written down where
it bites: `Select.Root` renders a 1×1 `aria-hidden` `<input>` for form serialisation (it is counted by any
`locator("input")` and by the contrast sweep); the focus trap redirects one `requestAnimationFrame` late, so
`components/ui/Dialog.tsx` keeps a synchronous Tab wrap on top; and `inert` is never set — containment is
`aria-hidden` on the other `<body>` children. Read those files before changing them.

A test drives a listbox with `chooseOption` / `chooseOptionByLabel` / `chosenValue` from `tests/profile.ts`.
Playwright's `selectOption()` and `inputValue()` only speak to a native `<select>`.

## Develop

```
pnpm install                # from the repo root (workspace)
printf %s 'a-long-console-password' | node apps/web-console/scripts/hash-password.mjs --write   # → .env.local
PALAI_BASE_URL=http://127.0.0.1:8080 PALAI_API_KEY=palai-sk-... \
  pnpm --filter @palai/web-console dev
```

## Prove

```
pnpm --filter @palai/web-console typecheck
pnpm --filter @palai/web-console test:e2e   # next build + playwright: auth + relay-gate + public-API-only + axe + keyboard + UI-002
```

`test:e2e` starts **two** consoles: the configured one, and a second identical process on port 3202 with **no**
`PALAI_CONSOLE_PASSWORD_HASH` — the fail-closed control, so "a console with no password does not serve" is
asserted against a real process rather than a mock. Neither needs a password from you: the Playwright config
runs the shipped `scripts/hash-password.mjs` to derive the hash it gives the first one, which is also what
binds the generator to the reader.

### Two profiles, one spec set (E19 T7)

`PALAI_CONSOLE_PROFILE` selects the `/v1` upstream. **Both profiles run the same spec files** — a spec that
can only pass on one of them is a *finding*, recorded in `tests/divergences.mjs`, never a reason to fork.

- **`fake`** (default) — the deterministic upstream (`tests/fake-control-plane.mjs`): fast, Docker-free, and
  the only place a hostile artifact, a scripted recovery or a paused approval can be produced on demand.
- **`real`** — a running compose control plane. The bootstrap key comes from the **environment**, never argv:

```
PALAI_DISPATCH_WORKERS=1 PALAI_MODEL_PROVIDER=fake palai local up
export PALAI_BASE_URL="$(jq -r .base_url "$PALAI_HOME/config.json")"
export PALAI_API_KEY="$(cat "$PALAI_HOME/api-key")"
pnpm --filter @palai/web-console test:e2e:real
pnpm --filter @palai/web-console sweep       # the fake-vs-real conformance sweep
```

Asking for the real profile without a real stack **fails the whole run** at config load. It never skips: a
suite that reports green for the absence of the thing it tests is worse than no suite.

### The conformance sweep (`pnpm sweep`, plan D15)

`tests/conformance.test.mjs` (node:test — no browser) diffs the objects the fixture *dispatches from* against
the surface it gathers from the **running real router**: route existence via `OPTIONS` (Go's ServeMux answers
405+`Allow` for a registered path, bare 404 for an unregistered one, so route-absence never masquerades as
resource-absence), response shapes and statuses, and the event vocabulary and payload keys of a **real run**.
Every difference must sit in `tests/divergences.mjs`; a row that *stops* being a difference fails too, so the
ledger cannot rot in either direction. It found that E17 T10's "invented approval event" is one instance of
six: five event types the fixture scripts are journaled by no production code, and three routes it serves are
not registered by the real router at all.

A **deployed** console (compose is not a deployment) plus a manual VoiceOver/screen-reader pass remains the
§6 operator leg above the automated a11y ceiling.

## Honest ceiling

Not a commercial SaaS UI (§5): no billing/team management. The operator console (cordon/drain/queue-inspect,
§47.4) is out — the E15 ops CLIs cover it. Config explainability is minimal (effective value only;
layer-by-layer attribution is a later iteration, §47.3). `console` is advertised **preview** in
`/v1/capabilities`; only the T11 exit gate may recompute it to stable.
