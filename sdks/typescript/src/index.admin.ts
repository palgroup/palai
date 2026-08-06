// Admin entrypoint (the "./admin" export condition, deliberately with NO "browser" condition —
// see package.json). It exposes the operator client, whose module chain imports the server-only
// guard via ./client.ts, so bundling this for the browser fails loud just like the default "."
// entrypoint. Unlike ".", there is no browser-safe fallback here: an operator surface has nothing
// a customer's browser should ever reach, so a bundler that cannot resolve "./admin" on the
// browser condition is working as intended, not missing a fallback.

export { PalaiAdmin } from "./admin-client.ts";
export type { PalaiOptions } from "./client.ts";

// Task 3: the same provisioning classes the root entrypoint exports, reachable here too — an
// admin-only consumer (who imports "@palai/sdk/admin" and never "@palai/sdk") can type a call
// against admin.projects/apiKeys without a second import for the param/result shapes.
export { ApiKeys, Projects } from "./resources/provisioning.ts";
export type {
  ApiKey,
  ApiKeyCreated,
  ApiKeyCreateParams,
  Project,
  ProjectCreateParams,
  ProjectPolicyParams,
} from "./resources/provisioning.ts";

// The FLEET surface, exported here for the reason the provisioning block above states: an admin-only
// consumer who imports "@palai/sdk/admin" and never "@palai/sdk" can type a call against
// admin.fleet.pools / poolKeys / runners without a second import for the param and result shapes.
//
// It is types only. The classes are reached through PalaiAdmin, which is what binds them to a client
// carrying a credential — exporting them as constructors here would invite a consumer to build one
// against a client that has none, and get a 401 instead of a compile error.
export type {
  DesiredConfig,
  MachineOccupancy,
  Runner,
  RunnerPool,
  RunnerPoolCreateParams,
  RunnerPoolKey,
  RunnerPoolKeyEnrollment,
  RunnerPoolKeyMintParams,
  RunnerPoolPatchParams,
} from "./resources/fleet.ts";
