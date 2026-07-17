// Server entrypoint (the default export condition). It exposes the API-key client, whose
// module chain imports the server-only guard, so bundling this for the browser fails loud.
// Browser code resolves ./index.browser instead (the "browser" export condition), which
// carries no credential path.

export { Palai, APIVersion } from "./client.ts";
export type { PalaiOptions, RequestOptions, ApiResult } from "./client.ts";

export { Responses } from "./resources/responses.ts";
export type { CreateOptions, RetrieveOptions, StreamOptions } from "./resources/responses.ts";

export { ResponseStream, isTerminalEvent, parseEventStream, fullJitterBackoff, delay } from "./stream.ts";
export type { SSEFrame, StreamTransport, ResponseStreamInit, StreamStart } from "./stream.ts";

export * from "./errors.ts";
export type * from "./generated/types.ts";
