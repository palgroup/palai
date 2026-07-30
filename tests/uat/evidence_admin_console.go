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
var AdminConsoleCaseIDs = []string{"CON-001", "CON-002", "CON-003", "CON-004"}
