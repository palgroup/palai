// Browser-safe entrypoint (the "browser" export condition). It exposes the typed transport
// shapes and the typed RFC 9457 error surface — everything a browser needs to render a
// response or narrow an error — but never the API-key client. The secret stays on the
// server; a browser that must read a live stream should proxy it through a server route.

export * from "./errors.ts";
export type * from "./generated/types.ts";
