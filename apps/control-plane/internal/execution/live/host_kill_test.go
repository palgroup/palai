//go:build live

// This file is CASE=host-kill-restore, the E10 Task 6 live smoke. A REAL provider run writes files into
// a REAL workspace, a pause cuts a checkpoint + a workspace byte-snapshot, the WHOLE HOST is killed (the
// runner daemon + its engine sandbox containers + host-path access — the local single-machine stand-in),
// a fresh runner enrolls and the workspace is RESTORED (snapshot checksums re-derived EQUAL), and the run
// continues to completion on the real provider. Evidence: the fence pair (old<new), the restore-checksum
// proof, the §26.12 RecoveryProof, and the fenced-out old host's stale authoritative frame DENIED.
//
// HONEST CEILINGS (spec §10.2), all named, none hidden:
//  1. "Whole host" is the single-machine approach: the engine sandbox container(s) + the runner process
//     are killed and the allocation reclaimed. A real multi-host fleet drain (a runner daemon on a
//     separate box, host-path severed at the network) is E14/E15.
//  2. The FENCING + snapshot-RESTORE-under-real-kill half is proven DETERMINISTICALLY against a real
//     container in tests/fault/recovery (host_kill_test.go: TestRunnerDaemonKillAdvancesFenceAndRecovers
//     + TestHostKillFencesStaleWriter) and against real S3+PG in the artifacts component tier
//     (TestHostMoveKeepsLogicalIdNewFencedAllocation). This live tier confirms the restored workspace
//     lets a REAL provider run RESUME and COMPLETE — resume-realness needs a real model, not more kills.
//  3. CONTAINER-ENGINE LIVE HARNESS GAP: tool advertising landed (E12 T1), so a real provider now reaches
//     a spontaneous tool/pause boundary through the uv-subprocess path (the checkpoint-restore /
//     container-kill-recovery smokes). What this whole-host smoke still needs is a container-engine live
//     harness: the engine running in a REAL container driven through the runner path, so a pause boundary
//     can be cut and the WHOLE HOST (runner + sandbox containers) killed. That harness is a NAMED FOLLOW-UP
//     — SKIP until it lands (the deterministic + component proofs above already own the mechanism).
//
// GATED: serialized with every LIVE/fault smoke on the shared :local Docker stack; NOT part of make
// verify / CI. Skips cleanly without creds. The credential is an opaque env-resolved secret, never printed.
package live

import (
	"testing"
)

// TestLiveHostKillRestoreRealProvider is CASE=host-kill-restore (see the file ceilings). It is authored
// with the deterministic + component proofs already green (Task 6); it activates once the container-engine
// live harness drives a pause boundary through the runner path (ceiling 3, a named follow-up).
func TestLiveHostKillRestoreRealProvider(t *testing.T) {
	_ = requireEnv(t, credentialEnv)
	_ = requireEnv(t, "PALAI_ENGINE_DIR")
	_ = requireEnv(t, "PALAI_COMPONENT_POSTGRES_URL")
	_ = requireEnv(t, "PALAI_S3_ENDPOINT")

	// Tool advertising landed (E12 T1); the remaining prerequisite is the container-engine live harness
	// (ceiling 3): the engine in a REAL container driven through the runner path, so a pause boundary can
	// be cut and the whole host killed. That harness is a named follow-up. Once it lands, this drives:
	// real provider file-writing run -> pause (checkpoint + workspace snapshot to real S3) -> whole-host
	// kill -> fresh runner + WorkspaceRecovery restore (checksums EQUAL) -> resume + completion on the real
	// provider, asserting the fence pair, the RecoveryProof, and the old host's stale-frame denial. The
	// mechanism is the T6 component proofs; SKIP (not FAIL) so no guaranteed-red case rides the known-list.
	t.Skip("host-kill-restore needs the container-engine live harness (engine in a real container, pause driven through the runner path) — a named follow-up. The fencing + restore-under-real-kill half is proven deterministically in tests/fault/recovery/host_kill_test.go and the artifacts component tier.")
}
