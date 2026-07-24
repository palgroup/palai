// server-only is a Next-specific build-time guard on top of the SDK's own runtime guard:
// importing this module (transitively, the API-key client) from a Client Component is a build
// error, so the credential path can never be bundled for the browser. This is the E13/E16
// server-relay stance the console inherits verbatim (examples/nextjs-sdk/lib/palai.ts).
import "server-only";

import { Palai } from "@palai/sdk";

let client: Palai | undefined;

// getPalaiClient builds the API-key client from server env, memoized. It fails fast when the
// credential or base URL is missing, so a misconfigured deploy errors on the first request
// rather than talking to the wrong place. The key lives ONLY here on the server; the sole
// callers are the relay Route Handlers (app/api/palai/**), which forward to the upstream /v1/*
// public API and re-project canonical bodies to the browser — never the credential.
//
// EDGE TRUST (E14 T7): to point the relay at a self-hosted TLS edge serving a private-CA cert, the
// only changes are PALAI_BASE_URL + the key, plus NODE_EXTRA_CA_CERTS=<ca.crt> in the Node env
// (Node's global fetch — the SDK's default transport — honors it). No relay code touches the credential.
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
