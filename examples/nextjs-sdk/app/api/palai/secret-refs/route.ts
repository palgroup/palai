import { problem } from "@/lib/relay";
import { rawBaseURL, rawHeaders } from "@/lib/raw";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// THE CREDENTIAL HALF OF THE PICKER. A private repository needs a token, and the ONE property this
// demo will not trade away is where that token lives: it is posted to this server route, forwarded
// to the control plane, and stored there. It is never returned, never rendered, and never held by
// the browser after the submit.
//
// MEASURED (apps/control-plane/internal/identity/secrets.go:80-90, :132): the create body is
// exactly {name, value}, decoded with DisallowUnknownFields — an extra field is a 400, unlike
// /v1/repository-bindings which ignores one. The 201 body is METADATA ONLY:
// {name, object:"secret_ref", version, updated_at}. There is no read-back of the value by design,
// so this route cannot leak one even if a later edit asked it to.
//
// WHAT THE NAME IS FOR: the name written here is the same string a repository binding carries as
// `connection_ref`. At publish time the control plane resolves that ref in the org's secret store
// (main.go:1107-1122) and pushes as THAT identity. An empty connection_ref means the deployment's
// GitHub App instead — a different identity, not a missing one, which is why the approval row says
// which of the two it will be rather than leaving the field blank.
export async function POST(request: Request): Promise<Response> {
  let body: Record<string, unknown>;
  try {
    body = (await request.json()) as Record<string, unknown>;
  } catch {
    return problem(400, "invalid_request", "the request body must be JSON");
  }

  const name = typeof body.name === "string" ? body.name.trim() : "";
  const value = typeof body.value === "string" ? body.value : "";
  if (name === "") {
    return problem(400, "invalid_request", "name is required");
  }
  if (value === "") {
    return problem(400, "invalid_request", "value is required");
  }

  try {
    const upstream = await fetch(`${rawBaseURL()}/v1/secret-refs`, {
      method: "POST",
      headers: rawHeaders(),
      // EXACTLY the two fields. The upstream decoder is strict, so a third would 400 the operator
      // for a mistake this route made.
      body: JSON.stringify({ name, value }),
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
