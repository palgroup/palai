import { problem } from "@/lib/relay";
import { rawBaseURL, rawHeaders } from "@/lib/raw";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// ---------------------------------------------------------------------------------------------
// WHAT THE REPOSITORY DID, ASSEMBLED FROM THE ONLY SURFACES THAT CARRY IT.
//
// The demo's centre panel shows a file tree, a shell transcript and a diff. NONE of that is on the
// event stream, and the honest thing is to say where it does come from rather than let three
// components imply a richness the journal does not have. MEASURED 2026-08-02:
//
//   tool_call.executing.v1 -> {run_id, replay_class, tool_call_id}
//   tool_call.completed.v1 -> {run_id, tool_call_id}
//
// That is the entire tool payload. No name, no arguments, no result, no path, no bytes. The
// durable `tool_calls` ledger has all of it, and NO HTTP ROUTE READS THAT LEDGER (RunToolCalls has
// exactly two consumers, both inside execution/changeset.go). The `changesets` table has only
// INSERT statements in storage/queries/changesets.sql — no SELECT, no route.
//
// So this route reads the two ARTIFACTS the run's changeset compiler writes at finalize
// (execution/changeset.go:109,112) and which /v1/responses/{id}/artifacts does serve:
//
//   logical_type "patch"        media_type text/x-diff     a real unified diff of repo/
//   logical_type "test-result"  media_type text/plain      the shell transcript
//
// TWO CONSEQUENCES THE SCREEN HAS TO STATE, because both make a component narrower than it looks:
//
//  1. THE FILE TREE CAN ONLY EVER SHOW FILES THE RUN CHANGED. Nothing enumerates a workspace from
//     outside the run — there is no /v1/workspaces route of any kind. A tree that also drew the
//     repository's untouched files would be drawing something no API returned.
//  2. BOTH ARTIFACTS EXIST ONLY AFTER THE RUN FINALIZES. CompileChangeset is called once, from
//     finalize.go:205. There is no during-the-run version of either, so the panel fills in at the
//     end rather than streaming. Claiming otherwise would be the "live" lie this tree keeps finding.
//
// AND ONE THAT COST A MEASUREMENT: a write OUTSIDE repo/ produces NO patch artifact at all. The
// diff is computed against the clone, so a file written to the allocation root is invisible to it.
// That is not a bug here, it is the reason the chat's system guidance tells the model to work in
// repo/ — see components/coding-chat.tsx.
// ---------------------------------------------------------------------------------------------

interface ArtifactMeta {
  id: string;
  logical_type?: string;
  media_type?: string;
  size_bytes?: number;
  run_id?: string;
}

export async function GET(request: Request): Promise<Response> {
  const responseId = new URL(request.url).searchParams.get("responseId")?.trim() ?? "";
  if (responseId === "") {
    return problem(400, "invalid_request", "responseId is required");
  }

  try {
    const listRes = await fetch(`${rawBaseURL()}/v1/responses/${encodeURIComponent(responseId)}/artifacts`, {
      headers: rawHeaders(),
      cache: "no-store",
    });
    if (!listRes.ok) {
      const text = await listRes.text();
      return new Response(text, {
        status: listRes.status,
        headers: { "Content-Type": listRes.headers.get("content-type") ?? "application/json", "Cache-Control": "no-store" },
      });
    }
    const list = (await listRes.json()) as { data?: ArtifactMeta[] };
    const artifacts = Array.isArray(list.data) ? list.data : [];

    const patchMeta = artifacts.find((a) => a.logical_type === "patch");
    const transcriptMeta = artifacts.find((a) => a.logical_type === "test-result");
    // The changeset's own summary (execution/changeset.go, logical_type "changeset-summary"). It rides
    // this same artifact list because the `changesets` table has no SELECT and no route, so it is the
    // only path by which anything outside the control plane learns what the changeset says.
    const summaryMeta = artifacts.find((a) => a.logical_type === "changeset-summary");

    const [patch, transcript, summaryBody] = await Promise.all([
      patchMeta ? fetchContent(patchMeta.id) : Promise.resolve(""),
      transcriptMeta ? fetchContent(transcriptMeta.id) : Promise.resolve(""),
      summaryMeta ? fetchContent(summaryMeta.id) : Promise.resolve(""),
    ]);
    const summary = parseSummary(summaryBody);

    return Response.json(
      {
        responseId,
        commands: parseTranscript(transcript),
        files: parsePatch(patch),
        patch,
        // How many .gitignore'd files the run wrote and the changeset deliberately did NOT list.
        // null means no summary artifact travelled, which is not the same as zero and must not be
        // rendered as one — a run whose changeset never compiled knows nothing about ignored files.
        ignoredFileCount: summary.ignoredFileCount,
        // Whether a changeset was compiled at all. Without it, "this run changed nothing" and "nothing
        // ever measured this run" are the same empty file list on screen.
        changesetCompiled: summaryMeta !== undefined,
        // The artifact ids are returned so the screen can name its own evidence. A panel that says
        // "the repository changed" without being able to say WHICH bytes said so is a decoration.
        artifacts: artifacts.map((a) => ({
          id: a.id,
          logicalType: a.logical_type ?? null,
          mediaType: a.media_type ?? null,
          sizeBytes: a.size_bytes ?? null,
        })),
        // MEASURED, and surfaced rather than left as a silent omission: the transcript's
        // logical_type is the literal "test-result" (changeset.go:112) even though it is a shell
        // transcript and not a test result. Naming it here keeps the demo from quietly repeating a
        // label it knows is wrong.
        notes: {
          transcriptLogicalType: transcriptMeta?.logical_type ?? null,
          settledOnly: true,
        },
      },
      { headers: { "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" } },
    );
  } catch (error) {
    return problem(502, "connection_error", error instanceof Error ? error.message : "the control plane is unreachable");
  }
}

// parseSummary reads the changeset summary artifact. It is deliberately total: a body that is absent,
// truncated or not JSON yields null rather than throwing, because the panel must still render the diff
// it already has. A malformed summary costs one sentence, not the screen.
export function parseSummary(body: string): { ignoredFileCount: number | null } {
  if (body.trim() === "") return { ignoredFileCount: null };
  try {
    const parsed = JSON.parse(body) as { ignored_file_count?: unknown };
    const count = parsed.ignored_file_count;
    return { ignoredFileCount: typeof count === "number" && Number.isFinite(count) ? count : null };
  } catch {
    return { ignoredFileCount: null };
  }
}

async function fetchContent(artifactId: string): Promise<string> {
  const res = await fetch(`${rawBaseURL()}/v1/artifacts/${encodeURIComponent(artifactId)}/content`, {
    headers: rawHeaders(),
    cache: "no-store",
  });
  return res.ok ? await res.text() : "";
}

export interface ShellCommand {
  command: string;
  exitCode: number | null;
  refused: string | null;
  output: string;
}

// parseTranscript reverses execution/changeset.go:196-224 exactly, and the boundary rule is the
// part worth reading.
//
// The writer emits, per shell call:  "$ <cmd>\n" then EITHER "refused: <why>\n" (and nothing else)
// OR "exit: <code>\n" followed by stdout and an optional "stderr: <text>\n".
//
// A naive split on lines starting with "$ " is WRONG: a command's own stdout can contain such a
// line (`echo '$ hello'`, a pasted transcript, a shell prompt in a README) and the panel would
// invent a command that never ran. The writer always puts exit/refused IMMEDIATELY after the
// command line, so THAT PAIR is the boundary — a "$ " line is only a command when the next line
// starts with "exit: " or "refused: ".
export function parseTranscript(text: string): ShellCommand[] {
  if (text.trim() === "") return [];
  const lines = text.split("\n");
  const out: ShellCommand[] = [];
  let current: ShellCommand | null = null;
  let buffer: string[] = [];

  const flush = () => {
    if (current !== null) {
      current.output = buffer.join("\n").replace(/\n+$/, "");
      out.push(current);
    }
    buffer = [];
  };

  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i] ?? "";
    const next = lines[i + 1] ?? "";
    const isBoundary = line.startsWith("$ ") && (next.startsWith("exit: ") || next.startsWith("refused: "));
    if (isBoundary) {
      flush();
      current = { command: line.slice(2), exitCode: null, refused: null, output: "" };
      if (next.startsWith("exit: ")) {
        const parsed = Number.parseInt(next.slice(6).trim(), 10);
        current.exitCode = Number.isNaN(parsed) ? null : parsed;
      } else {
        current.refused = next.slice(9).trim();
      }
      i += 1;
      continue;
    }
    if (current !== null) buffer.push(line);
  }
  flush();
  return out;
}

export interface ChangedFile {
  path: string;
  change: "added" | "modified" | "deleted";
  diff: string;
}

// parsePatch splits a unified diff into per-file entries. Paths come off the `+++ b/<path>` line
// (falling back to `--- a/<path>` for a deletion, where the +++ side is /dev/null), because the
// `diff --git a/x b/y` header quotes and escapes paths with spaces while the ---/+++ lines are what
// every diff consumer already reads.
export function parsePatch(patch: string): ChangedFile[] {
  if (patch.trim() === "") return [];
  const files: ChangedFile[] = [];
  const chunks = patch.split(/^diff --git /m).filter((c) => c.trim() !== "");
  for (const chunk of chunks) {
    const body = `diff --git ${chunk}`;
    const minus = /^--- (?:a\/)?(.+)$/m.exec(chunk);
    const plus = /^\+\+\+ (?:b\/)?(.+)$/m.exec(chunk);
    const minusPath = minus?.[1]?.trim() ?? "";
    const plusPath = plus?.[1]?.trim() ?? "";
    const deleted = plusPath === "/dev/null";
    const added = minusPath === "/dev/null" || /^new file mode/m.test(chunk);
    const path = deleted ? minusPath : plusPath;
    if (path === "" || path === "/dev/null") continue;
    files.push({ path, change: deleted ? "deleted" : added ? "added" : "modified", diff: body.replace(/\s+$/, "") });
  }
  return files;
}
