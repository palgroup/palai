import { problem } from "@/lib/relay";
import { rawBaseURL, rawHeaders } from "@/lib/raw";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// THE REPOSITORY PICKER'S SERVER HALF. The browser never holds the Palai credential; it asks this
// route, this route asks the control plane with the key that lives in the server's environment.
//
// WHY BINDINGS AND NOT "THE OPERATOR'S GITHUB REPOSITORIES", which is what a first reading of "pick
// a repo" suggests: listing someone's GitHub repositories needs an egress path to api.github.com
// and a user OAuth token, and this demo holds neither. It also would not help — a run can only be
// pointed at a REPOSITORY BINDING (`repository.binding_id` on POST /v1/responses), so a repository
// the operator picked off GitHub would have to become a binding before anything could clone it.
// The bind form below is that step, made visible instead of hidden behind a script.
//
// MEASURED 2026-08-02 against the live control plane, because the two verbs do NOT share a
// validation policy and a UI that assumes they do will mis-report the failure:
//
//   POST /v1/repository-bindings   provider, repository_identity, clone_url REQUIRED
//                                  clone_url must parse as http(s)  -> else 400
//                                  connection_ref is NOT validated at all — an unknown ref is
//                                    accepted here and fails later, at clone or publish time
//                                  json.Unmarshal, NO DisallowUnknownFields: an unknown field is
//                                    SILENTLY IGNORED rather than rejected
//   POST /v1/secret-refs           strictDecode -> an unknown field is a 400
//
// So this route sends exactly the documented fields and nothing else. A field it invented would be
// swallowed without complaint, which is the worst of the three outcomes: no effect and no error.

export async function GET(): Promise<Response> {
  try {
    const upstream = await fetch(`${rawBaseURL()}/v1/repository-bindings`, {
      headers: rawHeaders(),
      cache: "no-store",
    });
    const text = await upstream.text();
    return new Response(text, {
      status: upstream.status,
      headers: {
        "Content-Type": upstream.headers.get("content-type") ?? "application/json",
        "Cache-Control": "no-store",
        "X-Content-Type-Options": "nosniff",
      },
    });
  } catch (error) {
    return problem(502, "connection_error", error instanceof Error ? error.message : "the control plane is unreachable");
  }
}

export async function POST(request: Request): Promise<Response> {
  let body: Record<string, unknown>;
  try {
    body = (await request.json()) as Record<string, unknown>;
  } catch {
    return problem(400, "invalid_request", "the request body must be JSON");
  }

  const cloneURL = str(body.clone_url);
  const identity = str(body.repository_identity);
  const defaultBranch = str(body.default_branch);
  const connectionRef = str(body.connection_ref);

  if (cloneURL === "") {
    return problem(400, "invalid_request", "clone_url is required");
  }
  // THE SCHEME IS CHECKED HERE TOO, and not because the control plane fails to check it — it does,
  // at api/repository_bindings.go:91. It is checked here so the operator gets the sentence next to
  // the field they typed into rather than a relayed 400 at the bottom of the page. The control
  // plane remains the authority; this is an echo of its rule, not a substitute for it.
  if (!/^https?:\/\//i.test(cloneURL)) {
    return problem(400, "invalid_request", "clone_url must be an http(s) URL — ssh:// and git@ forms are refused by the control plane");
  }

  // repository_identity is REQUIRED upstream, and it is also the only thing that tells a pull
  // request which owner/repo to open against (the publisher splits it — approval.go:418). Deriving
  // it from the clone URL when the operator did not type one is a convenience, and the derivation
  // is shown back to them in the list rather than hidden.
  const derived = identity !== "" ? identity : deriveIdentity(cloneURL);
  if (derived === "") {
    return problem(400, "invalid_request", "repository_identity is required and could not be derived from clone_url");
  }

  try {
    const upstream = await fetch(`${rawBaseURL()}/v1/repository-bindings`, {
      method: "POST",
      headers: rawHeaders(),
      body: JSON.stringify({
        provider: "github",
        repository_identity: derived,
        clone_url: cloneURL,
        ...(defaultBranch !== "" ? { default_branch: defaultBranch } : {}),
        ...(connectionRef !== "" ? { connection_ref: connectionRef } : {}),
      }),
    });
    const text = await upstream.text();
    return new Response(text, {
      status: upstream.status,
      headers: {
        "Content-Type": upstream.headers.get("content-type") ?? "application/json",
        "Cache-Control": "no-store",
        "X-Content-Type-Options": "nosniff",
      },
    });
  } catch (error) {
    return problem(502, "connection_error", error instanceof Error ? error.message : "the control plane is unreachable");
  }
}

function str(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

// deriveIdentity turns https://github.com/owner/repo.git into owner/repo. It returns "" rather than
// a guess when the URL has no two-segment path, because a wrong owner/repo is how a pull request
// gets opened against a repository nobody meant.
function deriveIdentity(cloneURL: string): string {
  try {
    const path = new URL(cloneURL).pathname.replace(/^\/+/, "").replace(/\.git$/i, "");
    const parts = path.split("/").filter((p) => p !== "");
    return parts.length >= 2 ? `${parts[0]}/${parts[1]}` : "";
  } catch {
    return "";
  }
}
