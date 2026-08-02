"use client";

import { GitBranchIcon, KeyRoundIcon, PlusIcon, RefreshCwIcon, ShieldCheckIcon, ShieldOffIcon } from "lucide-react";
import { useCallback, useEffect, useId, useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

export interface Binding {
  id: string;
  repository_identity?: string;
  clone_url?: string;
  default_branch?: string;
  connection_ref?: string;
  provider?: string;
}

// THE PICKER. The owner's sentence is "ben repo seçeceğim demo ekrandan" — I will pick a repository
// from the demo screen — so the repository is chosen HERE, on screen, and the id it produces is what
// makes the next turn a coding session (`repository.binding_id` on POST /v1/responses).
//
// WHY THIS LISTS BINDINGS AND NOT THE OPERATOR'S GITHUB REPOSITORIES. A GitHub repository list needs
// egress to api.github.com and a user token; this demo holds neither. More to the point it would not
// shorten the path: a run can only be pointed at a BINDING, so any repository picked off GitHub would
// still have to be bound before a clone could happen. The bind form makes that step visible instead
// of hiding it in a shell script.
//
// EVERY FORM HERE CARRIES method="post". That is not decoration. This tree found ten forms whose
// tests posted to the endpoint and never submitted the form, and the sharpest one had no method at
// all: every moment before JavaScript attached, the browser's default GET put a PASSWORD IN THE URL.
// The credential form below carries a repository token. A form that leaks it into a history entry
// during hydration has leaked it.
export function RepositoryPicker({
  selectedId,
  onSelect,
  disabled,
}: {
  selectedId: string;
  onSelect: (binding: Binding | null) => void;
  disabled: boolean;
}) {
  const [bindings, setBindings] = useState<Binding[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [showBind, setShowBind] = useState(false);
  const [showSecret, setShowSecret] = useState(false);
  // Survives the form: see SecretForm for why the receipt must not live inside the thing that closes.
  const [lastSecret, setLastSecret] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const res = await fetch("/api/palai/bindings", { cache: "no-store" });
      const body = (await res.json()) as { data?: Binding[]; detail?: string };
      if (!res.ok) {
        setError(body.detail ?? `HTTP ${res.status}`);
        setBindings([]);
        return;
      }
      setBindings(Array.isArray(body.data) ? body.data : []);
    } catch (err) {
      setError(err instanceof Error ? err.message : "the binding list could not be read");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <div className="flex h-full flex-col gap-4 overflow-y-auto p-4" data-testid="repo-picker">
      <header className="flex items-baseline justify-between gap-2">
        <h2 className="font-medium text-[15px] text-foreground">Repository</h2>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          onClick={() => void load()}
          data-testid="repo-refresh"
          aria-label="Reload the repository list"
        >
          <RefreshCwIcon className={cn("size-3.5", loading && "palai-live")} />
        </Button>
      </header>

      <p className="text-[13px] text-muted-foreground leading-[18px]">
        Pick the repository this session writes to. The clone, the shell and any publication all
        happen inside it.
      </p>

      {error !== "" ? (
        <p data-testid="repo-error" className="rounded-md border border-destructive/40 bg-destructive/10 px-2 py-1.5 text-[13px] text-destructive">
          {error}
        </p>
      ) : null}

      <ul className="flex flex-col gap-1" data-testid="repo-list">
        {loading && bindings.length === 0 ? (
          <li className="px-2 py-1.5 text-[13px] text-muted-foreground">reading /v1/repository-bindings…</li>
        ) : null}
        {!loading && bindings.length === 0 && error === "" ? (
          <li data-testid="repo-empty" className="px-2 py-1.5 text-[13px] text-muted-foreground">
            No repository is bound yet. Bind one below — that is the step that gives a run something
            to clone.
          </li>
        ) : null}
        {bindings.map((b) => {
          const active = b.id === selectedId;
          return (
            <li key={b.id}>
              <button
                type="button"
                disabled={disabled}
                data-testid="repo-option"
                data-binding-id={b.id}
                aria-pressed={active}
                onClick={() => onSelect(active ? null : b)}
                className={cn(
                  "flex w-full flex-col gap-0.5 rounded-md px-2 py-1.5 text-left transition-colors",
                  "hover:bg-accent disabled:cursor-not-allowed disabled:opacity-50",
                  active ? "bg-accent text-foreground" : "text-ink-dim",
                )}
              >
                <span className="flex items-center gap-1.5">
                  <span
                    aria-hidden
                    className={cn("size-1.5 shrink-0 rounded-full", active ? "bg-brand" : "bg-border")}
                  />
                  <span className="truncate font-medium text-[14px]">{b.repository_identity || b.id}</span>
                </span>
                <span className="flex items-center gap-2 pl-3 text-[12px] text-muted-foreground">
                  <span className="inline-flex items-center gap-1">
                    <GitBranchIcon className="size-3" aria-hidden />
                    {b.default_branch || "—"}
                  </span>
                  {/* WHICH IDENTITY THIS BINDING WOULD PUBLISH AS, on the row where it is chosen
                      rather than only later on the approval. An empty connection_ref is not a
                      missing value — it means the deployment's GitHub App, a different identity —
                      so it gets its own words instead of a blank. */}
                  {b.connection_ref ? (
                    <span className="inline-flex items-center gap-1" title={`publishes as ${b.connection_ref}`}>
                      <ShieldCheckIcon className="size-3" aria-hidden />
                      <span className="truncate">{b.connection_ref}</span>
                    </span>
                  ) : (
                    <span className="inline-flex items-center gap-1" title="publishes as the deployment's GitHub App">
                      <ShieldOffIcon className="size-3" aria-hidden />
                      deployment App
                    </span>
                  )}
                </span>
              </button>
            </li>
          );
        })}
      </ul>

      <div className="flex flex-col gap-2 border-border border-t pt-3">
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="justify-start px-2 text-[13px] text-ink-dim"
          onClick={() => setShowBind((v) => !v)}
          aria-expanded={showBind}
          data-testid="repo-bind-toggle"
        >
          <PlusIcon className="size-3.5" aria-hidden /> Bind a repository
        </Button>
        {showBind ? <BindForm onBound={(b) => { void load(); onSelect(b); setShowBind(false); }} /> : null}

        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="justify-start px-2 text-[13px] text-ink-dim"
          onClick={() => setShowSecret((v) => !v)}
          aria-expanded={showSecret}
          data-testid="secret-toggle"
        >
          <KeyRoundIcon className="size-3.5" aria-hidden /> Add a credential
        </Button>
        {showSecret ? <SecretForm onCreated={setLastSecret} /> : null}
        {lastSecret !== "" ? (
          <p data-testid="secret-stored" className="px-2 text-[12px] text-muted-foreground">
            credential <span className="font-mono text-ink-dim">{lastSecret}</span> is stored — use that
            name as a binding&rsquo;s credential
          </p>
        ) : null}
      </div>
    </div>
  );
}

function BindForm({ onBound }: { onBound: (b: Binding) => void }) {
  const cloneId = useId();
  const identityId = useId();
  const branchId = useId();
  const refId = useId();
  const [state, setState] = useState<"idle" | "sending">("idle");
  const [error, setError] = useState("");

  return (
    <form
      method="post"
      action="/api/palai/bindings"
      data-testid="bind-form"
      className="flex flex-col gap-2 rounded-md border border-border bg-card p-3"
      onSubmit={async (e) => {
        e.preventDefault();
        const form = e.currentTarget;
        const data = new FormData(form);
        setState("sending");
        setError("");
        try {
          const res = await fetch("/api/palai/bindings", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              clone_url: String(data.get("clone_url") ?? ""),
              repository_identity: String(data.get("repository_identity") ?? ""),
              default_branch: String(data.get("default_branch") ?? ""),
              connection_ref: String(data.get("connection_ref") ?? ""),
            }),
          });
          const body = (await res.json()) as Binding & { detail?: string };
          if (!res.ok) {
            setError(body.detail ?? `HTTP ${res.status}`);
            return;
          }
          form.reset();
          onBound(body);
        } catch (err) {
          setError(err instanceof Error ? err.message : "the binding could not be created");
        } finally {
          setState("idle");
        }
      }}
    >
      <Field id={cloneId} label="Clone URL" hint="http(s) only — the control plane refuses ssh:// and git@ forms">
        <Input
          id={cloneId}
          name="clone_url"
          required
          data-testid="bind-clone-url"
          placeholder="https://github.com/owner/repo.git"
          className="h-8 text-[13px]"
        />
      </Field>
      <Field id={identityId} label="owner/repo" hint="optional — derived from the clone URL when blank; a pull request needs it">
        <Input id={identityId} name="repository_identity" data-testid="bind-identity" placeholder="owner/repo" className="h-8 text-[13px]" />
      </Field>
      <Field id={branchId} label="Base branch" hint="what a pull request would target">
        <Input id={branchId} name="default_branch" data-testid="bind-branch" placeholder="main" className="h-8 text-[13px]" />
      </Field>
      <Field id={refId} label="Credential name" hint="a secret-ref name — leave blank to publish as the deployment's GitHub App">
        <Input id={refId} name="connection_ref" data-testid="bind-connection-ref" placeholder="gh-pat-acme" className="h-8 text-[13px]" />
      </Field>
      {error !== "" ? (
        <p data-testid="bind-error" className="text-[12px] text-destructive">
          {error}
        </p>
      ) : null}
      <Button type="submit" size="sm" className="h-8 self-start text-[13px]" disabled={state === "sending"} data-testid="bind-submit">
        {state === "sending" ? "Binding…" : "Bind repository"}
      </Button>
    </form>
  );
}

// SecretForm posts the one value on this screen that is genuinely secret. It carries method="post"
// and action, so the pre-hydration submit is a POST to the route that already handles it rather than
// a GET that writes the token into the address bar. The field is type="password" and is never echoed
// back — the control plane's 201 body is metadata only ({name, object, version, updated_at}), so
// there is nothing to render even if this component wanted to.
function SecretForm({ onCreated }: { onCreated: (name: string) => void }) {
  const nameId = useId();
  const valueId = useId();
  const [state, setState] = useState<"idle" | "sending">("idle");
  const [error, setError] = useState("");
  const [created, setCreated] = useState("");

  return (
    <form
      method="post"
      action="/api/palai/secret-refs"
      data-testid="secret-form"
      className="flex flex-col gap-2 rounded-md border border-border bg-card p-3"
      onSubmit={async (e) => {
        e.preventDefault();
        const form = e.currentTarget;
        const data = new FormData(form);
        setState("sending");
        setError("");
        try {
          const res = await fetch("/api/palai/secret-refs", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ name: String(data.get("name") ?? ""), value: String(data.get("value") ?? "") }),
          });
          const body = (await res.json()) as { name?: string; version?: number; detail?: string };
          if (!res.ok) {
            setError(body.detail ?? `HTTP ${res.status}`);
            return;
          }
          form.reset();
          setCreated(`${body.name} (version ${body.version})`);
          // THE FORM STAYS OPEN, and the earlier version of this collapsed it here. That destroyed
          // the only confirmation the operator ever gets: the control plane's 201 body is metadata
          // only, so if this component unmounts its own "stored …" line there is NOTHING anywhere
          // saying the credential exists. Storing a secret and showing no receipt is how somebody
          // types it twice, or types it wrong and never learns.
          onCreated(String(body.name ?? ""));
        } catch (err) {
          setError(err instanceof Error ? err.message : "the credential could not be stored");
        } finally {
          setState("idle");
        }
      }}
    >
      <Field id={nameId} label="Name" hint="use this string as a binding's credential name">
        <Input id={nameId} name="name" required data-testid="secret-name" placeholder="gh-pat-acme" className="h-8 text-[13px]" />
      </Field>
      <Field id={valueId} label="Token" hint="stored by the control plane; never returned, never rendered">
        <Input id={valueId} name="value" type="password" required data-testid="secret-value" placeholder="ghp_…" className="h-8 text-[13px]" />
      </Field>
      {error !== "" ? (
        <p data-testid="secret-error" className="text-[12px] text-destructive">
          {error}
        </p>
      ) : null}
      {created !== "" ? (
        <p data-testid="secret-created" className="text-[12px] text-muted-foreground">
          stored {created}
        </p>
      ) : null}
      <Button type="submit" size="sm" className="h-8 self-start text-[13px]" disabled={state === "sending"} data-testid="secret-submit">
        {state === "sending" ? "Storing…" : "Store credential"}
      </Button>
    </form>
  );
}

function Field({ id, label, hint, children }: { id: string; label: string; hint: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      {/* Label ABOVE the input, full width — the measured dialog is single-column (design-reference
          §3: "Alan etiketi tam genişlik, girdi altında"), not the two-column settings layout. */}
      <label htmlFor={id} className="font-medium text-[13px] text-foreground">
        {label}
      </label>
      {children}
      <span className="text-[12px] text-muted-foreground leading-4">{hint}</span>
    </div>
  );
}
