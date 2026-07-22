// A minimal remote_http tool server built ENTIRELY on the Extension SDK, for the
// tool-sdk component smoke (spec §28.24, TOL-018). It verifies the invoke HMAC with
// extsdk.verify, answers 202, and posts an SDK-signed result callback
// (extsdk.callback + extsdk.callbackHeaders) to the invoke's callback.url bearing
// the one-use token. The Go harness drives a real executor.Invoke into it, so the
// signed round-trip runs through TWO SDK impls (Go signs the invoke, TS verifies;
// TS signs the callback, the Go control plane verifies).
//
// Config is env-only; the shared secret is in-memory bytes, never logged. The bound
// port is printed once ("LISTENING <port>") so the Go harness can dial it.
import { createServer, type IncomingMessage } from "node:http";

import {
  verify,
  callback,
  callbackHeaders,
  HEADER_ID,
  HEADER_TIMESTAMP,
  HEADER_SIGNATURE,
  HEADER_CALLBACK_TOKEN,
} from "../src/index.ts";

const secret = Buffer.from(process.env.TOOL_SDK_SECRET ?? "", "utf8");
const TOLERANCE_SECONDS = 300;

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

function header(req: IncomingMessage, name: string): string {
  const v = req.headers[name.toLowerCase()];
  return Array.isArray(v) ? (v[0] ?? "") : (v ?? "");
}

const server = createServer(async (req, res) => {
  if (req.method === "GET" && req.url === "/healthz") {
    res.writeHead(200);
    res.end("ok");
    return;
  }
  if (req.method !== "POST") {
    res.writeHead(405);
    res.end();
    return;
  }

  const raw = await readBody(req);
  const id = header(req, HEADER_ID);
  const ts = Number(header(req, HEADER_TIMESTAMP));
  const sig = header(req, HEADER_SIGNATURE);
  const now = Math.floor(Date.now() / 1000);

  // A bad/unsigned invoke never executes (verify-before-act).
  if (!Number.isFinite(ts) || !verify(secret, id, ts, raw, sig, now, TOLERANCE_SECONDS)) {
    res.writeHead(401);
    res.end();
    return;
  }

  let invoke: { tool_call_id?: string; callback?: { url?: string; token?: string } };
  try {
    invoke = JSON.parse(raw.toString("utf8"));
  } catch {
    res.writeHead(400);
    res.end();
    return;
  }

  // 202: the result comes later via the SDK-signed callback.
  res.writeHead(202);
  res.end();

  const url = invoke.callback?.url;
  const token = invoke.callback?.token;
  if (!url || !token) return;
  const operationId = url.split("/").pop() ?? "";
  const body = callback(operationId, invoke.tool_call_id ?? "", { answer: "sunny" });
  const headers: Record<string, string> = {
    ...callbackHeaders(operationId, now, body, secret),
    [HEADER_CALLBACK_TOKEN]: token,
    "content-type": "application/json",
  };
  await fetch(url, { method: "POST", headers, body }).catch(() => {});
});

server.listen(0, "127.0.0.1", () => {
  const addr = server.address();
  const port = typeof addr === "object" && addr ? addr.port : 0;
  process.stdout.write(`LISTENING ${port}\n`);
});
