// Admin entrypoint (the "./admin" export condition, deliberately with NO "browser" condition —
// see package.json). It exposes the operator client, whose module chain imports the server-only
// guard via ./client.ts, so bundling this for the browser fails loud just like the default "."
// entrypoint. Unlike ".", there is no browser-safe fallback here: an operator surface has nothing
// a customer's browser should ever reach, so a bundler that cannot resolve "./admin" on the
// browser condition is working as intended, not missing a fallback.

export { PalaiAdmin } from "./admin-client.ts";
export type { PalaiOptions } from "./client.ts";
