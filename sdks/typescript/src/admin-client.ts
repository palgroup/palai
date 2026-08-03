// Importing the Palai client marks this module server-only too (client.ts imports
// ./server-only.ts): bundling PalaiAdmin for the browser throws rather than shipping the
// operator's credential to a customer.
import { Palai, type PalaiOptions } from "./client.ts";
import { Fleet } from "./resources/fleet.ts";
import { ApiKeys, Organizations, Projects } from "./resources/provisioning.ts";

// PalaiAdmin is the operator client: it manages the fleet itself (projects, machines), not the
// per-project resources the API-key client exposes. It takes the same options as Palai and holds
// one internally rather than re-implementing the transport — composition over copying the
// request/retry/auth code, so a transport fix in Palai never needs a parallel fix here.
//
// `fleet` is the machine-fleet surface (Task 2): pools, their enrolment keys, and the enrolled
// machines. `organizations`/`projects`/`apiKeys` (Task 3) are the SAME provisioning classes the root
// entrypoint exposes on Palai — grouped here, not copied, each wired to this.#client exactly like
// Fleet is. Adding any of these earlier would have been a speculative surface with no caller.
export class PalaiAdmin {
  #client: Palai;
  readonly fleet: Fleet;
  readonly organizations: Organizations;
  readonly projects: Projects;
  readonly apiKeys: ApiKeys;

  constructor(options: PalaiOptions = {}) {
    this.#client = new Palai(options);
    this.fleet = new Fleet(this.#client);
    this.organizations = new Organizations(this.#client);
    this.projects = new Projects(this.#client);
    this.apiKeys = new ApiKeys(this.#client);
  }
}
