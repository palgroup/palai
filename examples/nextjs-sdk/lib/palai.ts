// server-only is a Next-specific build-time guard on top of the SDK's own runtime guard:
// importing this module (transitively, the API-key client) from a Client Component is a
// build error, so the credential path can never be bundled for the browser.
import "server-only";

import { Palai } from "@palai/sdk";

let client: Palai | undefined;

// getPalaiClient builds the API-key client from server env, memoized. It fails fast when
// the credential or base URL is missing, so a misconfigured deploy errors on the first
// request rather than silently talking to the wrong place. The key lives only here on the
// server; the sole caller (app/api/palai/route.ts) re-projects canonical events to the
// browser and never forwards the credential.
export function getPalaiClient(): Palai {
  if (client === undefined) {
    client = new Palai({
      apiKey: requiredEnv("PALAI_API_KEY"),
      baseURL: requiredEnv("PALAI_BASE_URL"),
    });
  }
  return client;
}

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(
      `${name} is required and is read server-side only. Set it before starting the app; ` +
        "it must never be exposed to the browser (no NEXT_PUBLIC_ prefix).",
    );
  }
  return value;
}
