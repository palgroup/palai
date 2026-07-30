package uat

// AdminConsoleCaseIDs are the UAT ids E25 opens, and the `CON-` prefix is a GATE decision rather than a
// naming preference. The plan measured why: `UI-` is already inside `extensionIDPrefixes`
// (tests/uat/extensions/catalog_test.go), and that map IS the shipped extensions-0.1.0 bundle's case list —
// the generator reads it, and uat.CapabilityClaims feeds a digest folded into that bundle's every checksum.
// So a `UI-005` would either force the regeneration of a committed historical release, rewriting the record
// of a run that happened, or fall through PromoteGateFor to a weaker family gate that knows none of E25's
// guards and would pass it. E20, E21 and E22 each paid for that once.
//
// OWNERSHIP LIVES HERE; ESCAPING THE SWEEP IS NOT ALLOWED. tests/uat/extensions is the ONLY place in the
// tree that walks the cases DIRECTORY, so `CON-` is added to its prefix list in the same change that adds
// this list — measured, not assumed: a CON-001 directory under an UNLISTED prefix was verified to leave
// every UAT gate green, which is precisely the hole E23 T2's HIL-004 fell into (a case directory nothing
// resolved proofs for, reported green by silence). The two directions are closed in two places:
// tests/uat/extensions refuses a CON- dir that no catalog owns, and tests/uat/admin-console refuses an id in
// this list that has no directory or whose declared proofs are not in the tree.
//
// THIS LIST GROWS ONE ID PER TASK, and only when the directory exists. T1 opens CON-001, T2 opens CON-002,
// T3 opens CON-003, T4 opens CON-004; the remaining ids the plan names (CON-005..007) join as their tasks
// land, and the E25 exit gate is where they meet a bundle.
//
// CON-003 IS THE FIRST NON-BROWSER CASE IN THIS LIST, and its proof_class says so: `component-real` rather
// than `e2e-deterministic`, because T3 is the Go half of R1 and touches no console file. Its proofs are Go
// test FUNCTIONS, so the catalog gate resolves them as `func Name(` rather than as `test("title"`.
//
// CON-004 IS ITS BROWSER HALF, and the pair is what R1 is: T3 proves an operator-authored value reaches a
// real shell command and enters no durable row; T4 proves the surface a HUMAN types it into returns it
// nowhere — in no DOM node, no response body and no source map. Neither claim is the other's, and neither
// is the ceiling both share: giving an agent a secret is the agent having that secret.
//
// CON-005 IS THE SMALLEST CASE IN THIS LIST AND ITS SMALLNESS IS THE MEASUREMENT (plan §3.6 D3): E23 T9 had
// already opened GET /v1/approvals, both decision routes, the `approve` capability and the mandatory request
// hash, so T5 is a PAGE. What it claims is bounded by one thing no console profile can produce — a PARKED
// gated tool call on a real stack (DIV-UI-006) — so the release of the parked run stays a component-tier
// claim and the browser's claim ends at the decision reaching the single throat with the row's own binding.
// CON-006 IS THE FIRST CASE IN THIS LIST WITH LEGS ON BOTH SIDES OF THE BROWSER, and the split is a
// measurement rather than a structure: T6's console can be shown creating a repository binding, an agent, a
// revision bound to an environment and a run pinned to it, but it can NEVER be shown the consequence — no
// console profile reaches a tool call at all (the fake upstream scripts its events; the compose adapter is
// hardcoded with no ToolCalls, DIV-UNX-001), and a browser cannot read a subprocess's environment anyway. So
// its last proof is a component-real Go test that drives the SAME public HTTP routes the screens call, with no
// SQL below the tenant seed, and then watches a real `printenv` receive the keys. That test is the one place
// in the tree where E25 T3's pipe and T6's console meet, and it is why neither task's claim is the other's.
// CON-007 IS THE ONLY CASE IN THIS LIST WHOSE FIRST MEASUREMENT WAS OF A SHIPPED DOCUMENT. T7's RED is not a
// missing feature, it is docs/operations/jira-mcp-connection.md §3 telling operators for three and a half days
// to publish a `$REV_ID` that no /v1 response has ever contained — and the step being unreachable is what made
// the E23 T5 decision hung on it (approval_required) unreachable too. Its component leg EXECUTES that runbook
// over the public API alone, which is the only way a document is verified: it was edited three times while it
// was wrong, so being read is not verification and being edited is not either. Its browser legs are the screen,
// and its untrusted-text leg is an ATTACK rather than an assertion — the discovered description carries a
// <script> and an <a>, and the spec then counts elements, anchors and navigations.
var AdminConsoleCaseIDs = []string{"CON-001", "CON-002", "CON-003", "CON-004", "CON-005", "CON-006", "CON-007"}
