// AGENT TEMPLATES — a starting revision, authored as YAML.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
// THE SHAPE THE OWNER ASKED FOR, AND WHAT IT MAPS ONTO HERE
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
//
// The requested template was copied from a reference console and reads:
//
//   name: Incident commander
//   description: Triages a Sentry alert, opens a Linear incident ticket, and runs the Slack war room.
//   model: claude-opus-4-8
//   system: |- …
//   mcp_servers:
//     - name: sentry
//       type: url
//       url: https://mcp.sentry.dev/mcp
//   tools:
//     - type: agent_toolset_20260401
//     - type: mcp_toolset
//       mcp_server_name: sentry
//       default_config:
//         permission_policy: { type: always_allow }
//   metadata:
//     template: incident-commander
//
// FIVE OF THOSE EIGHT KEYS HAVE NO COUNTERPART HERE, and a template that named them would be a template
// whose fields silently vanish. The whole mapping, measured 2026-08-01:
//
//   name          → agent_profiles.name, via POST /v1/agents.
//                   storage/migrations/000019_agents.up.sql:21, automation/agents.go:122 CreateProfile.
//
//   model         → RevisionInput.Model.      apps/control-plane/internal/automation/agents.go:77
//   system        → RevisionInput.Instructions (a rename, nothing more).            agents.go:79
//   tools         → RevisionInput.Tools — a FLAT LIST OF TOOL NAMES forming a capability ceiling, NOT a
//                   list of typed toolset objects.                                  agents.go:78
//   tool_sets     → RevisionInput.ToolSets — ids of PUBLISHED ToolSetRevisions.     agents.go:80
//   mcp_connections → RevisionInput.MCPConnections — ids of ALREADY-REGISTERED connections. agents.go:81
//   environment   → RevisionInput.Environment. The one rider that IS reference-checked at create and at
//                   publish.                                                        agents.go:87
//
//   description   → NOTHING. agent_profiles has four columns and none of them is a description
//                   (storage/migrations/000019_agents.up.sql:16-25). It is gallery copy on this screen and
//                   is NOT sent to the API — see buildRevisionBody, which drops it.
//
//   mcp_servers   → NOT AS WRITTEN. Theirs DECLARES a server inline, with a URL, and creating it is part of
//                   applying the template. Ours REFERENCES a connection that an admin already registered
//                   through POST /v1/mcp-connections (extensions/mcp.go:64 CreateMCPConnection), by id.
//                   A revision cannot bring a connection into existence, and it should not be able to:
//                   registration is an admin action carrying a trust level and a secret handle, and it runs
//                   an SSRF vet (extensions/mcp.go:80-86) that an agent-config write path does not.
//                   So a template names connections; /tools creates them. The template's own comments say
//                   which ones it wants.
//
//   permission_policy → NOTHING ON A REVISION, and `always_allow` is the exact hazard this console spends a
//                   screen on. The approval gate is decided PER TOOL at publish time — `approval_required`
//                   on the tool revision (extensions/registry.go:257), declared on /tools, defaulted ON for
//                   every tool (app/tools/page.tsx DEFAULT_DECLARATION) because "this console CANNOT KNOW
//                   which tool is a write tool". A template that could write `always_allow` would let a
//                   pasted YAML un-gate a write tool from a screen that never shows the tool's name.
//                   Its absence here is the feature.
//
//   metadata      → NOTHING, and it would be a 400 rather than a silent drop: DecodeRevisionInput sets
//                   json.DisallowUnknownFields (automation/agents.go:111-118), so any key outside
//                   RevisionInput is rejected by the server. That is also why this file validates key names
//                   itself — an operator editing YAML should learn about a typo here, not from a 400.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
// STORAGE DECISION: THE YAML IS AN AUTHORING FORMAT, NOT A STORED ARTEFACT
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
//
// A template is parsed into the existing two calls — POST /v1/agents then POST /v1/agents/{id}/revisions —
// and nothing about it is persisted. The gallery ships as the constant below.
//
// WHAT THAT COSTS: an operator cannot save their own template, and cannot share one with a teammate. That
// is a real capability and it is the thing a "template gallery" usually implies.
//
// WHY IT IS STILL RIGHT TODAY: the alternative is a tenant-scoped table, which in this tree is never just a
// table. It is a migration (whose number collides with whatever else is in flight), an RLS policy, a row in
// `allTables` or the tenancy corpus goes red (see the E13 T1 rule), a public API surface to read and write
// it, and a place for the YAML to disagree with the revision it produced — a second source of truth for
// agent config, which is the one thing the immutable-revision design exists to prevent. For a gallery of
// worked examples that ship with the console, that is a great deal of durable machinery to own.
//
// THE UPGRADE PATH IS CHEAP AND IT IS NOT BLOCKED BY THIS CHOICE: `parseTemplateYAML` takes a string from
// anywhere. The day templates become operator-authored, the parser and the editor are already written; only
// the source of the string changes.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
// WHY THE PARSER IS OURS, AND WHAT IT REFUSES
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
//
// This console has no YAML dependency (package.json, 2026-08-01: six runtime deps, none of them a parser)
// and adding one to render a settings file is a supply-chain decision that outweighs the feature. So the
// TEMPLATE SCHEMA IS DELIBERATELY FLAT — scalars, block scalars, and lists of scalars, which is exactly
// what RevisionInput holds — and the reader below handles that grammar and REFUSES everything else by name.
//
// It is not a YAML implementation and must never be described as one. It does not do flow collections
// (`[a, b]`), anchors, multiple documents, nested maps, or lists of maps. Each of those produces a numbered
// error rather than a wrong parse, which is the property that matters: the failure mode of a partial parser
// is silent misreading, and refusing loudly is how this one avoids it.

/** The keys a template may set. Anything else is an error at parse time, before the server sees it. */
const TEMPLATE_KEYS = [
  "name",
  "description",
  "model",
  "system",
  "tools",
  "tool_sets",
  "mcp_connections",
  "environment",
] as const;

type TemplateKey = (typeof TEMPLATE_KEYS)[number];

/** The keys whose value is a list of scalars. The rest are scalars or block scalars. */
const LIST_KEYS = ["tools", "tool_sets", "mcp_connections"] as const;
type ListKey = (typeof LIST_KEYS)[number];
type ScalarKey = Exclude<TemplateKey, ListKey>;

/**
 * The two accumulators are kept SEPARATE and typed rather than written through one
 * `Record<string, unknown>`, because a single bag would have made every assignment below a cast — and a
 * cast is exactly what stops the compiler noticing that a list key was written a string.
 */
const isListKey = (key: TemplateKey): key is ListKey => (LIST_KEYS as readonly string[]).includes(key);

export interface ParsedTemplate {
  name: string;
  description: string;
  model: string;
  system: string;
  tools: string[];
  tool_sets: string[];
  mcp_connections: string[];
  environment: string;
}

export interface ParseError {
  /** 1-based, so it matches the gutter the editor renders. */
  line: number;
  message: string;
}

export interface ParseResult {
  template: ParsedTemplate | null;
  errors: ParseError[];
}

const EMPTY: ParsedTemplate = {
  name: "",
  description: "",
  model: "",
  system: "",
  tools: [],
  tool_sets: [],
  mcp_connections: [],
  environment: "",
};

/** Strips one trailing inline comment from a scalar. Quoted values keep their `#`. */
function stripComment(value: string): string {
  if (value.startsWith('"') || value.startsWith("'")) return value;
  const hash = value.indexOf(" #");
  return (hash === -1 ? value : value.slice(0, hash)).trim();
}

/** Unwraps one layer of matching quotes, so `name: "A: B"` survives the colon. */
function unquote(value: string): string {
  const quoted =
    (value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"));
  return quoted && value.length >= 2 ? value.slice(1, -1) : value;
}

/**
 * Reads the template grammar described in this file's header.
 *
 * It returns EVERY error it finds rather than the first, because an editor that reveals one mistake per
 * save is an editor an operator fights. `template` is non-null only when `errors` is empty.
 */
export function parseTemplateYAML(source: string): ParseResult {
  const errors: ParseError[] = [];
  const scalars: Record<ScalarKey, string> = {
    name: EMPTY.name,
    description: EMPTY.description,
    model: EMPTY.model,
    system: EMPTY.system,
    environment: EMPTY.environment,
  };
  const lists: Record<ListKey, string[]> = { tools: [], tool_sets: [], mcp_connections: [] };
  const seen = new Set<TemplateKey>();

  const lines = source.split("\n");
  let i = 0;

  while (i < lines.length) {
    const raw = lines[i];
    const lineNo = i + 1;
    const trimmed = raw.trim();

    // Blank lines and whole-line comments carry nothing.
    if (trimmed === "" || trimmed.startsWith("#")) {
      i += 1;
      continue;
    }

    if (raw.startsWith(" ") || raw.startsWith("\t")) {
      errors.push({
        line: lineNo,
        message: "Unexpected indentation — a template key must start at column 1.",
      });
      i += 1;
      continue;
    }

    if (trimmed.startsWith("- ")) {
      errors.push({ line: lineNo, message: "A list item here has no key above it." });
      i += 1;
      continue;
    }

    const colon = raw.indexOf(":");
    if (colon === -1) {
      errors.push({ line: lineNo, message: `Not a \`key: value\` line: ${JSON.stringify(trimmed)}.` });
      i += 1;
      continue;
    }

    const key = raw.slice(0, colon).trim();
    const rest = raw.slice(colon + 1).trim();

    if (!(TEMPLATE_KEYS as readonly string[]).includes(key)) {
      errors.push({
        line: lineNo,
        message: `Unknown field \`${key}\`. A template may set: ${TEMPLATE_KEYS.join(", ")}.`,
      });
      i += 1;
      continue;
    }
    const field = key as TemplateKey;

    if (seen.has(field)) {
      errors.push({ line: lineNo, message: `\`${field}\` is set more than once.` });
    }
    seen.add(field);

    // ── a block scalar: `system: |-` ────────────────────────────────────────────────────────────────
    if (rest === "|" || rest === "|-" || rest === ">" || rest === ">-") {
      if (rest.startsWith(">")) {
        errors.push({
          line: lineNo,
          message: "Folded blocks (`>`) are not supported here — use `|-` so the text is kept verbatim.",
        });
      }
      const body: string[] = [];
      i += 1;
      let indent = -1;
      while (i < lines.length) {
        const blockLine = lines[i];
        if (blockLine.trim() === "") {
          body.push("");
          i += 1;
          continue;
        }
        const lead = blockLine.length - blockLine.trimStart().length;
        if (lead === 0) break;
        if (indent === -1) indent = lead;
        body.push(blockLine.slice(indent));
        i += 1;
      }
      while (body.length > 0 && body[body.length - 1] === "") body.pop();
      const text = body.join("\n");
      if (isListKey(field)) {
        errors.push({ line: lineNo, message: `\`${field}\` is a list, not a block of text.` });
      } else {
        scalars[field] = text;
      }
      continue;
    }

    // ── a list: `tools:` then `  - name` ────────────────────────────────────────────────────────────
    if (rest === "") {
      if (!isListKey(field)) {
        errors.push({
          line: lineNo,
          message: `\`${field}\` needs a value on the same line (or \`|-\` to start a block).`,
        });
      }
      const items: string[] = [];
      i += 1;
      while (i < lines.length) {
        const itemLine = lines[i];
        const itemTrim = itemLine.trim();
        if (itemTrim === "" || itemTrim.startsWith("#")) {
          i += 1;
          continue;
        }
        if (!itemLine.startsWith(" ") && !itemLine.startsWith("\t")) break;
        if (!itemTrim.startsWith("- ")) {
          errors.push({
            line: i + 1,
            message: `Expected a \`- item\` under \`${field}\`. Nested objects are not supported — see this template's comments for why.`,
          });
          i += 1;
          continue;
        }
        const item = unquote(stripComment(itemTrim.slice(2).trim()));
        if (item === "") {
          errors.push({ line: i + 1, message: "An empty list item." });
        } else {
          items.push(item);
        }
        i += 1;
      }
      if (isListKey(field)) lists[field] = items;
      continue;
    }

    // ── a plain scalar ──────────────────────────────────────────────────────────────────────────────
    if (isListKey(field)) {
      errors.push({
        line: lineNo,
        message: `\`${field}\` is a list — put each entry on its own \`- \` line beneath it.`,
      });
      i += 1;
      continue;
    }
    scalars[field] = unquote(stripComment(rest));
    i += 1;
  }

  if (scalars.name.trim() === "") {
    errors.push({
      line: 1,
      // POST /v1/agents refuses a nameless profile — store/agents.go MissingName. Catching it here means the
      // operator reads the reason next to the field rather than as a 400 after two round trips.
      message: "`name` is required — POST /v1/agents refuses a profile without one.",
    });
  }

  return { template: errors.length === 0 ? { ...scalars, ...lists } : null, errors };
}

/**
 * The revision body, exactly as POST /v1/agents/{id}/revisions takes it.
 *
 * `name` and `description` are DROPPED, and that is the mapping being honoured rather than a bug: the name
 * belongs to the profile (a different call), and the description belongs to nothing — agent_profiles has no
 * such column. Every remaining key is a RevisionInput field, and an empty one is omitted rather than sent
 * empty, so a template that sets nothing produces a revision that imposes no ceiling.
 */
export function buildRevisionBody(t: ParsedTemplate): Record<string, unknown> {
  const body: Record<string, unknown> = {};
  if (t.model.trim() !== "") body.model = t.model.trim();
  if (t.system.trim() !== "") body.instructions = t.system;
  if (t.tools.length > 0) body.tools = t.tools;
  if (t.tool_sets.length > 0) body.tool_sets = t.tool_sets;
  if (t.mcp_connections.length > 0) body.mcp_connections = t.mcp_connections;
  if (t.environment.trim() !== "") body.environment = t.environment.trim();
  return body;
}

export interface AgentTemplate {
  id: string;
  title: string;
  /** One sentence for the gallery card. */
  summary: string;
  /** The MCP connection names this template expects, shown as the card's integration row. */
  integrations: string[];
  yaml: string;
}

// THE GALLERY.
//
// EVERY TEMPLATE NAMES ONLY CONNECTABLE SERVERS, and that constraint changed what these say. The obvious
// first template is the owner's own worked example — Sentry alert → Linear ticket → Slack war room — and
// TWO of its three integrations cannot be dialled from this deployment: Slack is OAuth-only, and Sentry
// wants a `Sentry-Bearer` scheme our transport cannot render (see lib/mcpCatalogue.ts for both, with
// citations). Shipping it as written would put a template in front of an operator that cannot be applied,
// which is the same failure as a catalogue with a button that fails at the end.
//
// So the incident template below is built on GitHub and Linear, which are both static-token servers, and it
// says in its own comments what it left out and why.
export const AGENT_TEMPLATES: readonly AgentTemplate[] = [
  {
    id: "incident-commander",
    title: "Incident commander",
    summary:
      "Triages an alert, opens a Linear incident ticket, and keeps the timeline as the investigation moves.",
    integrations: ["github", "linear"],
    yaml: `# INCIDENT COMMANDER
#
# Registers nothing. \`mcp_connections\` names connections that must ALREADY exist — register them on
# /tools first, then paste their ids here (they look like mcpc_…).
#
# NOT SENTRY, AND NOT SLACK, though an incident agent wants both. Neither can be connected from this
# deployment: Slack's MCP server takes only an OAuth user token, and Sentry authenticates on a
# \`Sentry-Bearer\` scheme this transport cannot send. Both are listed on /tools with the reason.
name: Incident commander
description: Triages an alert, opens a Linear incident ticket, and keeps the timeline.
model: claude-opus-4-8
system: |-
  You are an on-call incident commander.

  Establish what broke, when it started, and what changed immediately before. Open ONE Linear issue for
  the incident and keep it as the running record: every finding goes in a comment, not in a new issue.

  State your confidence. If the evidence does not support a cause, say the evidence does not support a
  cause — a plausible story told firmly is the expensive failure in an incident, not silence.
tool_sets:
  # Published tool-set revision ids from /tools. A set is what puts tools in front of the model.
  - tsr_replace_me
mcp_connections:
  - mcpc_replace_me_github
  - mcpc_replace_me_linear
`,
  },
  {
    id: "repo-archaeologist",
    title: "Repository archaeologist",
    summary:
      "Answers “why is this code like this” from history and documentation, with no credential to register.",
    integrations: ["deepwiki", "context7"],
    yaml: `# REPOSITORY ARCHAEOLOGIST
#
# THE ONE TEMPLATE THAT NEEDS NO CREDENTIAL. Both servers it expects are public: DeepWiki and Context7 each
# answer an unauthenticated handshake (probed 2026-08-01 — see lib/mcpCatalogue.ts). Register them on
# /tools with no secret_ref at all, then paste their ids here.
name: Repository archaeologist
description: Explains why code is the way it is, from history and documentation.
model: claude-opus-4-8
system: |-
  You explain why a codebase is the way it is.

  Prefer the record over inference: a commit message, an issue, a design document. When you cannot find a
  record, say that the reason is not written down anywhere you can see — do not reconstruct a plausible
  motive and present it as history.

  Quote what you found, with its source, so the reader can check you.
mcp_connections:
  - mcpc_replace_me_deepwiki
  - mcpc_replace_me_context7
`,
  },
  {
    id: "release-notes",
    title: "Release notes writer",
    summary: "Turns merged pull requests into notes a customer can read, grouped by what changed for them.",
    integrations: ["github"],
    yaml: `# RELEASE NOTES WRITER
#
# Needs one GitHub connection (a Personal Access Token — see /tools). Nothing else.
name: Release notes writer
description: Turns merged pull requests into notes a customer can read.
model: claude-opus-4-8
system: |-
  You write release notes for people who use the product, not for people who wrote it.

  Group by what changed for the reader — not by component, and never by pull request number. Lead each
  entry with the effect, then the detail. A refactor with no user-visible effect belongs in a single line
  at the end, or nowhere.

  Never describe a change you cannot point at a merged pull request for.
tool_sets:
  - tsr_replace_me
mcp_connections:
  - mcpc_replace_me_github
`,
  },
];
