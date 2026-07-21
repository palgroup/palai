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
//  3. CONTAINER-ENGINE LIVE HARNESS GAP (shared with checkpoint-restore / container-kill-recovery): this
//     package runs the engine as a uv-subprocess, and dispatchModel does not yet advertise tool schemas
//     to the provider, so a real provider never reaches a tool/pause boundary through the container
//     runner path. SKIP until PALAI_LIVE_TOOL_ADVERTISING is set once that wiring lands.
//
// GATED: serialized with every LIVE/fault smoke on the shared :local Docker stack; NOT part of make
// verify / CI. Skips cleanly without creds. The credential is an opaque env-resolved secret, never printed.
package live

import (
	"os"
	"testing"
)

// TestLiveHostKillRestoreRealProvider is CASE=host-kill-restore (see the file ceilings). It is authored
// with the deterministic + component proofs already green (Task 6); it activates once the container-engine
// live harness advertises tools and drives a pause boundary through the runner path.
func TestLiveHostKillRestoreRealProvider(t *testing.T) {
	_ = requireEnv(t, credentialEnv)
	_ = requireEnv(t, "PALAI_ENGINE_DIR")
	_ = requireEnv(t, "PALAI_COMPONENT_POSTGRES_URL")
	_ = requireEnv(t, "PALAI_S3_ENDPOINT")

	if os.Getenv("PALAI_LIVE_TOOL_ADVERTISING") == "" {
		t.Skip("host-kill-restore needs the container-engine live harness + tool-advertising (dispatchModel omits Tools); set PALAI_LIVE_TOOL_ADVERTISING=1 once wired. The fencing + restore-under-real-kill half is proven deterministically in tests/fault/recovery/host_kill_test.go and the artifacts component tier.")
	}

	// Once the harness wiring lands, this drives: real provider file-writing run -> pause (checkpoint +
	// workspace snapshot to real S3) -> whole-host kill -> fresh runner + WorkspaceRecovery restore
	// (checksums EQUAL) -> resume + completion on the real provider, asserting the fence pair, the
	// RecoveryProof, and the old host's stale-frame denial. The mechanism is the T6 component proofs.
	t.Fatal("host-kill-restore live harness not yet wired; see ceiling 3 (should be unreachable until PALAI_LIVE_TOOL_ADVERTISING is set)")
}
