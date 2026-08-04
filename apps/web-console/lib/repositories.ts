// The repository-binding row, as contracts.RepositoryBinding actually projects it.
//
// EVERY FIELD BELOW IS OPTIONAL AND THAT IS THE WIRE RATHER THAN CAUTION: the create handler marshals with
// `omitempty` per field (the fixture mirrors it field for field, tests/fake-control-plane.mjs), so a binding
// registered without a connection ref, without allowed operations, without a policy, without a data
// classification or without a region constraint has those keys ABSENT — not empty. A console that rendered
// `""` for an absent key would be claiming somebody set it to nothing.
//
// `created_at` IS ON THIS WIRE, unlike an agent's. That asymmetry is worth naming because it decides a
// column: contracts.RepositoryBinding carries the timestamp in its own body, while an agent's rides
// api.ListRow and is spent minting a pagination cursor (lib/agents.ts holds that measurement). So this
// screen has a Created column and /agents cannot have one.

export interface BindingRow extends Record<string, unknown> {
  id?: string;
  provider?: string;
  repository_identity?: string;
  clone_url?: string;
  default_branch?: string;
  connection_ref?: string;
  allowed_operations?: unknown;
  policy?: unknown;
  data_classification?: string;
  region_constraint?: string;
  created_at?: string;
  /**
   * PRESENT ONLY ON A RETIRED BINDING (E30, migration 000057). `omitempty` on the wire, so a live binding
   * carries no key at all rather than a null — and the difference matters here: `archived_at === undefined`
   * is the live state, and a component testing truthiness of an empty string would get it right by accident
   * and wrong the day the field starts being emitted as "".
   */
  archived_at?: string;
  // organization_id was declared here beside project_id until A.2 Task 6 dropped the column; the
  // projection no longer carries it and nothing read it.
  project_id?: string;
}

/** SecretRefRow is GET /v1/secret-refs' row: a NAME, a version, a timestamp — and never a value. */
export interface SecretRefRow {
  name?: string;
  version?: number;
}

/** operationsOf renders `allowed_operations`, which is an array on the wire and absent when none were set. */
export function operationsOf(row: BindingRow): string {
  return Array.isArray(row.allowed_operations) && row.allowed_operations.length > 0 ? row.allowed_operations.join(", ") : "";
}
