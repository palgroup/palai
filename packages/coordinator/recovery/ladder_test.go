package recovery

import "testing"

// compatibleCandidate is a checkpoint that passes every §26.4 compatibility condition against
// compatibleTarget — the fixture each table row perturbs by exactly one condition.
func compatibleCandidate() Candidate {
	return Candidate{
		Present:             true,
		Format:              "reference-kernel",
		FormatVersion:       1,
		RecordedChecksum:    "sha256:abc",
		ComputedChecksum:    "sha256:abc",
		ConfigSnapshotHash:  "cfg-1",
		ProtocolVersion:     "engine.v1",
		TranscriptSequence:  7,
		WorkspaceSnapshotID: "", // no workspace dependency (T4: §26.4 workspace condition vacuous)
	}
}

func compatibleTarget() Target {
	return Target{
		OriginalLeaseAlive:  false,
		SupportedFormats:    []string{"reference-kernel/1"},
		ConfigSnapshotHash:  "cfg-1",
		ProtocolVersion:     "engine.v1",
		JournalSequence:     9,
		TranscriptAvailable: true,
	}
}

// TestLadderDecision pins the §26.3 order and the §26.4 seven-condition compatibility decision:
// an original-live lease wins exact; a candidate passing all seven wins compatible_checkpoint;
// dropping any ONE condition falls to transcript reconstruction; and a policy that forbids
// reconstruction turns an incompatible candidate into an explicit failure (never a silent drop).
func TestLadderDecision(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Candidate, *Target)
		want   Level
		wantOK bool // wants an empty Failures list
	}{
		{
			name:   "original lease alive wins exact before any checkpoint read",
			mutate: func(_ *Candidate, tg *Target) { tg.OriginalLeaseAlive = true },
			want:   LevelExact, wantOK: true,
		},
		{
			name:   "seven conditions pass wins compatible checkpoint",
			mutate: func(_ *Candidate, _ *Target) {},
			want:   LevelCompatibleCheckpoint, wantOK: true,
		},
		{
			name:   "unknown format name falls to transcript",
			mutate: func(_ *Candidate, tg *Target) { tg.SupportedFormats = []string{"other-kernel/1"} },
			want:   LevelTranscriptReconstruction,
		},
		{
			name:   "unsupported format version falls to transcript",
			mutate: func(c *Candidate, _ *Target) { c.FormatVersion = 2 },
			want:   LevelTranscriptReconstruction,
		},
		{
			name:   "checksum mismatch falls to transcript",
			mutate: func(c *Candidate, _ *Target) { c.ComputedChecksum = "sha256:tampered" },
			want:   LevelTranscriptReconstruction,
		},
		{
			name:   "config changed falls to transcript",
			mutate: func(_ *Candidate, tg *Target) { tg.ConfigSnapshotHash = "cfg-2" },
			want:   LevelTranscriptReconstruction,
		},
		{
			name:   "protocol incompatible falls to transcript",
			mutate: func(_ *Candidate, tg *Target) { tg.ProtocolVersion = "engine.v2" },
			want:   LevelTranscriptReconstruction,
		},
		{
			name:   "boundary ahead of journal falls to transcript",
			mutate: func(c *Candidate, tg *Target) { tg.JournalSequence = c.TranscriptSequence - 1 },
			want:   LevelTranscriptReconstruction,
		},
		{
			name:   "unrestorable workspace dependency falls to transcript",
			mutate: func(c *Candidate, _ *Target) { c.WorkspaceSnapshotID = "wsnap_1"; c.WorkspaceRestorable = false },
			want:   LevelTranscriptReconstruction,
		},
		{
			name:   "policy forbids reconstruction turns incompatible into explicit failure",
			mutate: func(c *Candidate, tg *Target) { c.ComputedChecksum = "sha256:tampered"; tg.ReconstructionForbidden = true },
			want:   LevelExplicitFailure,
		},
		{
			name:   "no checkpoint and no transcript is an explicit failure",
			mutate: func(c *Candidate, tg *Target) { c.Present = false; tg.TranscriptAvailable = false },
			want:   LevelExplicitFailure,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, tg := compatibleCandidate(), compatibleTarget()
			tc.mutate(&c, &tg)
			got := Decide(c, tg)
			if got.Level != tc.want {
				t.Fatalf("Decide level = %q, want %q (failures %v)", got.Level, tc.want, got.Failures)
			}
			if tc.wantOK && len(got.Failures) != 0 {
				t.Fatalf("Decide for %q reported failures %v, want none", tc.want, got.Failures)
			}
			if !tc.wantOK && tc.want != LevelExact && len(got.Failures) == 0 {
				t.Fatalf("Decide for %q reported no failure reasons; a non-exact fallback must name why", tc.want)
			}
		})
	}
}

// TestWorkspaceDependencyVacuousWhenAbsent guards the T4 honest ceiling: a checkpoint that
// declares NO workspace dependency (WorkspaceSnapshotID == "") satisfies the §26.4 workspace
// condition even though snapshot RESTORE is T6 — the condition passes vacuously, it is not skipped.
func TestWorkspaceDependencyVacuousWhenAbsent(t *testing.T) {
	c, tg := compatibleCandidate(), compatibleTarget()
	c.WorkspaceSnapshotID = ""
	c.WorkspaceRestorable = false // irrelevant when there is no dependency
	if got := Decide(c, tg); got.Level != LevelCompatibleCheckpoint {
		t.Fatalf("absent workspace dependency must pass vacuously, got %q %v", got.Level, got.Failures)
	}
}
