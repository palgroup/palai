import { problem } from "@/lib/relay";
import { clearedSessionCookie, sessionCookie, verifyPassword } from "@/lib/session";

// The SDK is not imported here and neither is the API key — this route touches only the password hash. It
// still runs on the Node runtime because scrypt does.
export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// THE ONE NON-/api/palai/ SAME-ORIGIN PATH (§2). Everything else the browser can address goes through the
// /v1 relay, and public-api-only.spec.ts asserts that this is the SINGLE exception — that no request outside
// it leaves the relay prefix, and that this path carries NO Bearer, because a session is not a credential
// for the control plane and the console's key must never ride a browser request.
//
// It is also the one route deliberately exempt from requireSession, for the obvious reason that it is how a
// session is obtained; tests/relay-gate.spec.ts names that exemption once, by path, and counts it — so the
// exemption cannot grow to two without a red test.
export async function POST(request: Request): Promise<Response> {
  // The Origin check runs here too. A login POST is a mutation (it mints a session), and it is the one
  // mutation an attacker's page would most like to trigger.
  const origin = request.headers.get("origin");
  const host = request.headers.get("host");
  if (origin === null || host === null || safeHost(origin) !== host) {
    return problem(403, "origin_mismatch", "a sign-in must come from the console's own origin");
  }

  let password = "";
  try {
    const body = (await request.json()) as { password?: unknown };
    if (typeof body.password === "string") password = body.password;
  } catch {
    // A malformed body is an empty password: it still pays for a full scrypt derivation below, so a
    // probe cannot learn from the response time whether its JSON parsed.
  }

  if (!(await verifyPassword(password))) {
    // ONE answer for every failure. Not "unknown operator", not "wrong password", not "console not
    // configured" — this console has a single credential, and a refusal that distinguishes its cases is a
    // refusal that helps a guesser. The 503 an unconfigured console returns is on the RELAY (requireSession),
    // where the operator sees it and an attacker learns nothing they could not learn from an empty page.
    return new Response(JSON.stringify({ type: "https://docs.palai.dev/problems/authentication_required", title: "authentication_required", status: 401, code: "authentication_required", detail: "the password was not accepted" }), {
      status: 401,
      headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store", "Set-Cookie": clearedSessionCookie() },
    });
  }

  // 204: the session IS the response. Nothing about the operator, the key or the upstream is echoed back.
  return new Response(null, { status: 204, headers: { "Cache-Control": "no-store", "Set-Cookie": sessionCookie() } });
}

function safeHost(origin: string): string | null {
  try {
    return new URL(origin).host;
  } catch {
    return null;
  }
}
