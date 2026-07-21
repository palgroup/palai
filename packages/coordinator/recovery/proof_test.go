package recovery

import "testing"

// completeProof is a §26.12 RecoveryProof with every field group populated — the fixture
// TestRecoveryProofComplete perturbs one field at a time to prove each is load-bearing.
func completeProof() RecoveryProof {
	return RecoveryProof{
		PreviousAttemptID:    "att_prev",
		NewAttemptID:         "att_new",
		Level:                LevelCompatibleCheckpoint,
		CheckpointID:         "chk_1",
		WorkspaceSnapshotID:  "", // no workspace dependency (T4 ceiling); empty is honest, not missing
		TranscriptBoundaryID: "bnd_1",
		ReplayedToolCalls:    []string{}, // empty list is itself the evidence: nothing was replayed
		ReusedToolCalls:      []string{"tcall_a"},
		ConfigModelChanges:   []string{}, // no config/model drift across the recovery
		SemanticLossAssessed: true,
		SemanticLossWarning:  "",
		DurationMS:           42,
	}
}

// TestRecoveryProofComplete pins §26.12: a proof with all eight field groups is Complete;
// dropping any one required field makes it invalid. A "resumed" log is never evidence on its
// own (REC-006) — Complete is the gate the verifier rings.
func TestRecoveryProofComplete(t *testing.T) {
	if !completeProof().Complete() {
		t.Fatal("a fully-populated §26.12 proof must be Complete")
	}

	cases := []struct {
		name string
		zero func(*RecoveryProof)
	}{
		{"missing previous attempt id", func(p *RecoveryProof) { p.PreviousAttemptID = "" }},
		{"missing new attempt id", func(p *RecoveryProof) { p.NewAttemptID = "" }},
		{"previous and new attempt id identical", func(p *RecoveryProof) { p.NewAttemptID = p.PreviousAttemptID }},
		{"missing level", func(p *RecoveryProof) { p.Level = "" }},
		{"missing checkpoint id", func(p *RecoveryProof) { p.CheckpointID = "" }},
		{"missing transcript boundary id", func(p *RecoveryProof) { p.TranscriptBoundaryID = "" }},
		{"replayed tool calls not accounted", func(p *RecoveryProof) { p.ReplayedToolCalls = nil }},
		{"reused tool calls not accounted", func(p *RecoveryProof) { p.ReusedToolCalls = nil }},
		{"config/model changes not accounted", func(p *RecoveryProof) { p.ConfigModelChanges = nil }},
		{"semantic loss not assessed", func(p *RecoveryProof) { p.SemanticLossAssessed = false }},
		{"duration not measured", func(p *RecoveryProof) { p.DurationMS = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := completeProof()
			tc.zero(&p)
			if p.Complete() {
				t.Fatalf("proof missing %q must NOT be Complete", tc.name)
			}
		})
	}
}
