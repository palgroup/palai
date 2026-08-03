// Importing the Palai client marks this module server-only too (client.ts imports
// ./server-only.ts): bundling PalaiAdmin for the browser throws rather than shipping the
// operator's credential to a customer.
import { Palai, type PalaiOptions } from "./client.ts";

// PalaiAdmin is the operator client: it manages the fleet itself (projects, machines), not the
// per-project resources the API-key client exposes. It takes the same options as Palai and holds
// one internally rather than re-implementing the transport — composition over copying the
// request/retry/auth code, so a transport fix in Palai never needs a parallel fix here.
//
// It carries no resources yet: Task 2 adds `fleet`, Task 3 groups `provisioning` under it. Adding
// either earlier would be a speculative surface with no caller.
export class PalaiAdmin {
  #client: Palai;

  constructor(options: PalaiOptions = {}) {
    this.#client = new Palai(options);
  }
}
