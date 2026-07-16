import { once } from "node:events";
import { createServer } from "node:http";

export const FIRST_FRAME =
  'id: event-001\nevent: progress\ndata: {"step":1}\n\n';
export const TERMINAL_FRAME =
  'id: event-002\nevent: completed\ndata: {"step":2}\n\n';
export const RESUMED_FRAME =
  'id: event-003\nevent: resumed\ndata: {"step":3}\n\n';
export const TERMINAL_DELAY_MS = 350;

export async function startFakeUpstream() {
  let mode = "complete";
  let cancelCalls = 0;
  const requests = [];

  const server = createServer((request, response) => {
    const url = new URL(request.url ?? "/", "http://upstream.invalid");
    if (url.pathname === "/cancel") {
      cancelCalls += 1;
      response.writeHead(204).end();
      return;
    }
    if (url.pathname !== "/stream") {
      response.writeHead(404).end("not found");
      return;
    }

    let resolveClosed;
    const closed = new Promise((resolve) => {
      resolveClosed = resolve;
    });
    const record = {
      authorization: request.headers.authorization ?? null,
      closed,
      closedAt: null,
      firstWrittenAt: null,
      lastEventId: request.headers["last-event-id"] ?? null,
      method: request.method ?? "",
      mode,
      rawHeaders: [...request.rawHeaders],
      terminalWrittenAt: null,
      url: request.url ?? "",
    };
    requests.push(record);

    response.once("close", () => {
      record.closedAt = performance.now();
      resolveClosed(record.closedAt);
    });

    if (mode === "error") {
      response.writeHead(401, {
        "content-type": "text/plain; charset=utf-8",
        "x-upstream-authorization": record.authorization ?? "",
      });
      response.end(`rejected ${record.authorization ?? "missing"}`);
      return;
    }

    response.writeHead(200, {
      "cache-control": "no-cache, no-transform",
      connection: "keep-alive",
      "content-type": "text/event-stream; charset=utf-8",
    });
    response.flushHeaders();

    if (mode === "resume") {
      record.firstWrittenAt = performance.now();
      response.end(RESUMED_FRAME);
      return;
    }

    record.firstWrittenAt = performance.now();
    response.write(FIRST_FRAME);
    if (mode === "hold") {
      return;
    }

    const timer = setTimeout(() => {
      if (!response.destroyed) {
        record.terminalWrittenAt = performance.now();
        response.end(TERMINAL_FRAME);
      }
    }, TERMINAL_DELAY_MS);
    response.once("close", () => clearTimeout(timer));
  });

  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("fake upstream did not bind a TCP port");
  }

  return {
    get cancelCalls() {
      return cancelCalls;
    },
    requests,
    setMode(nextMode) {
      if (!["complete", "error", "hold", "resume"].includes(nextMode)) {
        throw new Error("unsupported fake upstream mode");
      }
      mode = nextMode;
    },
    async stop() {
      server.closeAllConnections();
      server.close();
      await once(server, "close");
    },
    url: `http://127.0.0.1:${address.port}/stream`,
  };
}
