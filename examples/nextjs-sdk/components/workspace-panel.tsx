"use client";

import { FileDiffIcon, FilePlusIcon, FileXIcon, GitBranchIcon, TerminalIcon } from "lucide-react";
import { useEffect, useState } from "react";

import {
  CodeBlock,
  CodeBlockActions,
  CodeBlockCopyButton,
  CodeBlockFilename,
  CodeBlockHeader,
  CodeBlockTitle,
} from "@/components/ai-elements/code-block";
import {
  FileTree,
  FileTreeActions,
  FileTreeFile,
  FileTreeFolder,
  FileTreeIcon,
  FileTreeName,
} from "@/components/ai-elements/file-tree";
import {
  Terminal,
  TerminalContent,
  TerminalHeader,
  TerminalTitle,
} from "@/components/ai-elements/terminal";
import { Badge } from "@/components/ui/badge";
import type { Binding } from "@/components/repository-picker";
import { cn } from "@/lib/utils";

interface ShellCommand {
  command: string;
  exitCode: number | null;
  refused: string | null;
  output: string;
}
interface ChangedFile {
  path: string;
  change: "added" | "modified" | "deleted";
  diff: string;
}
interface WorkspaceView {
  responseId: string;
  commands: ShellCommand[];
  files: ChangedFile[];
  patch: string;
  artifacts: { id: string; logicalType: string | null; mediaType: string | null; sizeBytes: number | null }[];
  notes: { transcriptLogicalType: string | null; settledOnly: boolean };
}

// ---------------------------------------------------------------------------------------------
// THE SUBJECT OF THE SCREEN.
//
// The brief's design instruction is the whole argument for this file: the interesting thing is not
// a chat bubble, it is the repository changing while you watch. So the repository gets the middle
// and the largest share of the page, and the chat is its control surface on the right.
//
// Everything below is read from ARTIFACTS, and the panel says so out loud, because the alternative
// is three components implying a live feed that does not exist. What is true, measured 2026-08-02:
//
//   * the shell transcript and the diff are written ONCE, by CompileChangeset at run finalize
//     (execution/finalize.go:205). There is no during-the-run form of either. The panel therefore
//     fills in when the run settles; it does not stream, and it does not pretend to.
//   * NO API enumerates a workspace. There is no /v1/workspaces route of any kind, the tool_calls
//     ledger has no reader, and storage/queries/changesets.sql contains only INSERTs. So the tree
//     below is the set of files THIS RUN CHANGED and can never be the repository's whole contents.
//     The heading says "changed in this run" rather than "files", which is the difference between
//     a narrow true statement and a wide false one.
// ---------------------------------------------------------------------------------------------
export function WorkspacePanel({
  binding,
  responseId,
  running,
  sessionId,
}: {
  binding: Binding | null;
  responseId: string | null;
  running: boolean;
  sessionId: string | null;
}) {
  const [view, setView] = useState<WorkspaceView | null>(null);
  const [selected, setSelected] = useState<string>("");
  const [loadError, setLoadError] = useState("");

  // The fetch is keyed on (responseId, running): it fires when a run settles, which is the first
  // moment the artifacts exist.
  useEffect(() => {
    if (responseId === null || running) return;
    let cancelled = false;
    (async () => {
      try {
        const res = await fetch(`/api/palai/workspace?responseId=${encodeURIComponent(responseId)}`, { cache: "no-store" });
        const body = (await res.json()) as WorkspaceView & { detail?: string };
        if (cancelled) return;
        if (!res.ok) {
          setLoadError(body.detail ?? `HTTP ${res.status}`);
          return;
        }
        setLoadError("");
        setView(body);
        setSelected((prev) => (prev !== "" && body.files.some((f) => f.path === prev) ? prev : (body.files[0]?.path ?? "")));
      } catch (err) {
        if (!cancelled) setLoadError(err instanceof Error ? err.message : "the run's artifacts could not be read");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [responseId, running]);

  const files = view?.files ?? [];
  const commands = view?.commands ?? [];
  const selectedFile = files.find((f) => f.path === selected) ?? null;

  return (
    <section className="flex h-full min-w-0 flex-col" data-testid="workspace-panel" aria-label="Repository">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-border border-b px-5 py-3">
        <h1 className="font-medium text-[22px] text-foreground leading-7" data-testid="workspace-title">
          {binding?.repository_identity ?? "No repository selected"}
        </h1>
        {binding ? (
          <span className="inline-flex items-center gap-1 text-[13px] text-muted-foreground">
            <GitBranchIcon className="size-3.5" aria-hidden />
            {binding.default_branch || "—"}
          </span>
        ) : null}
        <span className="ml-auto flex items-center gap-2 text-[13px] text-muted-foreground">
          {running ? (
            <>
              <span aria-hidden className="palai-live size-2 rounded-full bg-brand" />
              <span data-testid="workspace-status">working</span>
            </>
          ) : (
            <span data-testid="workspace-status">{responseId ? "settled" : "idle"}</span>
          )}
        </span>
      </header>

      {binding === null ? (
        <p className="px-5 py-4 text-[14px] text-ink-dim" data-testid="workspace-no-repo">
          Pick a repository on the left. Until one is chosen a turn still runs, but it has no clone,
          no shell and nothing to publish — the tools answer &ldquo;no workspace bound for this
          run&rdquo;.
        </p>
      ) : null}

      {loadError !== "" ? (
        <p data-testid="workspace-error" className="mx-5 mt-4 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-[13px] text-destructive">
          {loadError}
        </p>
      ) : null}

      <div className="grid min-h-0 flex-1 grid-cols-1 gap-0 overflow-hidden lg:grid-cols-[240px_minmax(0,1fr)]">
        {/* ------------------------------------------------------------------ the file tree */}
        <div className="min-h-0 overflow-y-auto border-border border-b p-3 lg:border-r lg:border-b-0">
          <h2 className="mb-2 px-1 font-medium text-[13px] text-ink-dim">Changed in this run</h2>
          {files.length === 0 ? (
            <p data-testid="tree-empty" className="px-1 text-[12px] text-muted-foreground leading-4">
              {responseId === null
                ? "Nothing yet."
                : running
                  ? "The diff is written when the run settles."
                  : "This run changed no file inside the clone."}
              <br />
              <span className="text-muted-foreground/80">
                This tree lists only what the run changed. Palai exposes no route that enumerates a
                workspace, so the repository&rsquo;s untouched files cannot be shown here without
                inventing them.
              </span>
            </p>
          ) : (
            <FileTree
              defaultExpanded={new Set(["repo"])}
              selectedPath={selected}
              onSelect={setSelected}
              data-testid="file-tree"
            >
              {/* The clone lives at <allocation>/repo and the diff's paths are relative to it, so
                  the folder is drawn explicitly rather than implied — a path that reads
                  "GREETING.md" alone would suggest the workspace root, which is a different
                  directory and the one a shell command actually starts in. */}
              <FileTreeFolder name="repo" path="repo">
                {files.map((f) => (
                  <FileTreeFile key={f.path} name={f.path} path={f.path}>
                    <span className="size-4 shrink-0" />
                    <FileTreeIcon>
                      {f.change === "added" ? (
                        <FilePlusIcon className="size-4 text-added" aria-hidden />
                      ) : f.change === "deleted" ? (
                        <FileXIcon className="size-4 text-removed" aria-hidden />
                      ) : (
                        <FileDiffIcon className="size-4 text-muted-foreground" aria-hidden />
                      )}
                    </FileTreeIcon>
                    <FileTreeName>{f.path}</FileTreeName>
                    <FileTreeActions>
                      <Badge
                        variant="secondary"
                        className={cn(
                          "px-1 py-0 text-[10px]",
                          f.change === "added" && "text-added",
                          f.change === "deleted" && "text-removed",
                        )}
                      >
                        {f.change}
                      </Badge>
                    </FileTreeActions>
                  </FileTreeFile>
                ))}
              </FileTreeFolder>
            </FileTree>
          )}
        </div>

        {/* -------------------------------------------------- the diff and the shell transcript */}
        <div className="min-h-0 space-y-4 overflow-y-auto p-4">
          {selectedFile !== null ? (
            <CodeBlock code={selectedFile.diff} language="diff" data-testid="file-diff">
              <CodeBlockHeader>
                <CodeBlockTitle>
                  <CodeBlockFilename data-testid="file-diff-name">repo/{selectedFile.path}</CodeBlockFilename>
                </CodeBlockTitle>
                <CodeBlockActions>
                  <CodeBlockCopyButton />
                </CodeBlockActions>
              </CodeBlockHeader>
            </CodeBlock>
          ) : null}

          <div>
            <h2 className="mb-2 flex items-center gap-1.5 font-medium text-[13px] text-ink-dim">
              <TerminalIcon className="size-3.5" aria-hidden /> Shell
            </h2>
            {commands.length === 0 ? (
              <p data-testid="shell-empty" className="text-[12px] text-muted-foreground leading-4">
                {responseId === null
                  ? "No command has run yet."
                  : running
                    ? "The transcript is written when the run settles."
                    : "This run ran no shell command."}
              </p>
            ) : (
              <ol className="space-y-3" data-testid="shell-list">
                {commands.map((c, i) => (
                  <li key={`${i}-${c.command}`}>
                    {/* AI Elements' Terminal takes ONE `output` string and has no command prop and
                        no exit-code prop. Rather than fold the command into the output string and
                        lose it as data, the header carries the command and the exit code — the two
                        facts the transcript records per call — and TerminalContent renders only
                        what the command printed. */}
                    <Terminal output={c.output} data-testid="shell-command">
                      <TerminalHeader>
                        <TerminalTitle>
                          <span className="font-mono text-[12px]" data-testid="shell-cmd">
                            {c.command}
                          </span>
                        </TerminalTitle>
                        <span className="ml-auto shrink-0 font-mono text-[12px]" data-testid="shell-exit">
                          {c.refused !== null ? (
                            <span className="text-destructive">refused: {c.refused}</span>
                          ) : (
                            <span className={c.exitCode === 0 ? "text-muted-foreground" : "text-destructive"}>
                              exit {c.exitCode ?? "?"}
                            </span>
                          )}
                        </span>
                      </TerminalHeader>
                      {c.output !== "" ? <TerminalContent /> : null}
                    </Terminal>
                  </li>
                ))}
              </ol>
            )}
          </div>

          {/* THE EVIDENCE, NAMED. A panel that says "the repository changed" without being able to
              point at the bytes that said so is decoration. These are the artifact ids a reader can
              curl for themselves. */}
          {view !== null && view.artifacts.length > 0 ? (
            <footer className="border-border border-t pt-3 text-[12px] text-muted-foreground" data-testid="workspace-evidence">
              <p className="mb-1">Read from GET /v1/responses/{view.responseId}/artifacts:</p>
              <ul className="space-y-0.5 font-mono">
                {view.artifacts.map((a) => (
                  <li key={a.id}>
                    {a.id} · {a.logicalType} · {a.mediaType} · {a.sizeBytes}B
                  </li>
                ))}
              </ul>
              {view.notes.transcriptLogicalType === "test-result" ? (
                <p className="mt-1.5 text-muted-foreground/80" data-testid="workspace-mislabel">
                  The shell transcript is stored with logical_type &ldquo;test-result&rdquo;. That is
                  the literal Palai writes (changeset.go:112); it is a transcript, not a test result.
                </p>
              ) : null}
            </footer>
          ) : null}

          {sessionId !== null ? (
            <p className="font-mono text-[11px] text-muted-foreground/70" data-testid="workspace-session">
              session {sessionId}
            </p>
          ) : null}
        </div>
      </div>
    </section>
  );
}
