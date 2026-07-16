import { createServer, type IncomingMessage, type ServerResponse } from "node:http";

type Options = {
  port: number;
  heartbeatMs: number;
};

const options = parseOptions(process.argv.slice(2));
const activeStreams = new Set<ServerResponse>();
let disconnects = 0;
let cancelRequests = 0;
let stopping = false;

const server = createServer((request, response) => {
  const url = new URL(request.url ?? "/", "http://127.0.0.1");
  if (url.pathname === "/healthz") {
    if (request.method !== "GET") return methodNotAllowed(response);
    response.writeHead(200, { "content-type": "text/plain; charset=utf-8" });
    response.end("ok\n");
    return;
  }
  if (url.pathname === "/stats") {
    if (request.method !== "GET") return methodNotAllowed(response);
    response.writeHead(200, { "content-type": "application/json" });
    response.end(
      JSON.stringify({
        active_connections: activeStreams.size,
        disconnects,
        cancel_requests: cancelRequests,
      }),
    );
    return;
  }
  if (url.pathname === "/jobs/fixture/cancel") {
    if (request.method !== "POST") return methodNotAllowed(response);
    cancelRequests += 1;
    response.writeHead(202);
    response.end();
    return;
  }
  if (url.pathname === "/events") {
    if (request.method !== "GET") return methodNotAllowed(response);
    serveEvents(request, response);
    return;
  }
  response.writeHead(404);
  response.end();
});

server.on("clientError", (_error, socket) => socket.destroy());
server.listen(options.port, "127.0.0.1", () => {
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("listener did not expose a TCP address");
  }
  process.stdout.write(
    `${JSON.stringify({ event: "ready", address: `${address.address}:${address.port}`, runtime: "node" })}\n`,
  );
  process.stderr.write(`control-plane candidate listening runtime=node address=${address.address}:${address.port}\n`);
});

process.on("SIGINT", () => shutdown("SIGINT"));
process.on("SIGTERM", () => shutdown("SIGTERM"));

function serveEvents(request: IncomingMessage, response: ServerResponse): void {
  let lastID: number;
  try {
    lastID = parseLastEventID(request.headers["last-event-id"]);
  } catch (error) {
    response.writeHead(400, { "content-type": "text/plain; charset=utf-8" });
    response.end(`${error instanceof Error ? error.message : "invalid Last-Event-ID"}\n`);
    return;
  }

  response.writeHead(200, {
    "content-type": "text/event-stream; charset=utf-8",
    "cache-control": "no-cache",
    connection: "keep-alive",
  });
  response.flushHeaders();
  activeStreams.add(response);

  for (let sequence = lastID + 1; sequence <= 2; sequence += 1) {
    response.write(`id: ${sequence}\nevent: fixture\ndata: {"sequence":${sequence}}\n\n`);
  }
  const heartbeat = setInterval(() => response.write(": heartbeat\n\n"), options.heartbeatMs);
  response.once("close", () => {
    clearInterval(heartbeat);
    activeStreams.delete(response);
    disconnects += 1;
  });
}

function parseLastEventID(value: string | string[] | undefined): number {
  if (value === undefined || value === "") return 0;
  if (Array.isArray(value)) throw new Error("invalid Last-Event-ID");
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < 0 || parsed > 2) {
    throw new Error("invalid Last-Event-ID");
  }
  return parsed;
}

function methodNotAllowed(response: ServerResponse): void {
  response.writeHead(405);
  response.end();
}

function shutdown(signal: string): void {
  if (stopping) return;
  stopping = true;
  process.stderr.write(`control-plane candidate stopping runtime=node signal=${signal}\n`);
  for (const response of activeStreams) response.end();
  server.close((error) => {
    if (error) {
      process.stderr.write(`control-plane candidate shutdown error=${error.message}\n`);
      process.exitCode = 1;
    }
  });
  setTimeout(() => {
    server.closeAllConnections();
    process.exitCode = 1;
  }, 4_500).unref();
}

function parseOptions(args: string[]): Options {
  let port = 0;
  let heartbeatMs = 15_000;
  for (const argument of args) {
    if (argument.startsWith("--port=")) {
      port = parseInteger(argument.slice("--port=".length), "port");
    } else if (argument.startsWith("--heartbeat=")) {
      heartbeatMs = parseDuration(argument.slice("--heartbeat=".length));
    } else {
      throw new Error(`unknown argument: ${argument}`);
    }
  }
  if (port < 0 || port > 65_535 || heartbeatMs <= 0) throw new Error("invalid port or heartbeat");
  return { port, heartbeatMs };
}

function parseInteger(value: string, name: string): number {
  const parsed = Number(value);
  if (!Number.isInteger(parsed)) throw new Error(`invalid ${name}`);
  return parsed;
}

function parseDuration(value: string): number {
  if (value.endsWith("ms")) return parseInteger(value.slice(0, -2), "heartbeat");
  if (value.endsWith("s")) return parseInteger(value.slice(0, -1), "heartbeat") * 1_000;
  throw new Error("heartbeat must use ms or s");
}
