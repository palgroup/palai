// E14 T7 self-host journey — the nextjs-sdk relay's SERVER-SIDE SDK path, driven against a production
// self-host TLS edge with ONLY a base-URL/key/CA change. It builds the SAME @palai/sdk client lib/palai.ts
// builds (apiKey + baseURL) and streams a response to a REAL completion — the load-bearing "the SDK reaches
// the real stack through the edge" proof. The full Next.js HTTP wrapper + browser projection is the example's
// own Playwright ceiling (fake upstream); this harness is the SDK half.
//
// EDGE TRUST: the edge CA is trusted the zero-dependency way lib/palai.ts documents — NODE_EXTRA_CA_CERTS in
// this process's environment (Node's global fetch, the SDK's default transport, honors it). The journey sets
// it. Server-side only: the credential + CA path never leave this process.
import { Palai } from "@palai/sdk";

function requiredEnv(name) {
  const v = process.env[name]?.trim();
  if (!v) throw new Error(`${name} is required (server-side only)`);
  return v;
}

const client = new Palai({ apiKey: requiredEnv("PALAI_API_KEY"), baseURL: requiredEnv("PALAI_BASE_URL") });

const stream = client.responses.stream({ input: "Reply with the single word: ready." });
for await (const _event of stream) {
  // Drain the canonical event stream (the relay re-projects these; here we only need the terminal).
}
if (!stream.responseID) throw new Error("stream produced no response id");
const final = await client.responses.retrieve(stream.responseID);
if (final.status !== "completed") {
  throw new Error(`run reached ${final.status}, want completed`);
}
// Print ONLY non-secret canonical fields (never the raw provider payload) — the relay's projectFinal shape.
console.log(`sdk-edge-run completed: model=${final.model} status=${final.status}`);
