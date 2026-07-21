package recovery

// RecoveryProof is the §26.12 evidence a recovery claim must carry. A journal that merely says
// "resumed"/"continued" is NEVER evidence on its own (REC-006): only a Complete proof is. The
// eight field groups are the attempt pair, the chosen level, the checkpoint/snapshot ids, the
// transcript boundary, the replayed-vs-reused tool calls, the config/model changes, the
// semantic-loss assessment, and the measured duration.
type RecoveryProof struct {
	PreviousAttemptID    string
	NewAttemptID         string
	Level                Level
	CheckpointID         string
	WorkspaceSnapshotID  string   // "" is honest when the checkpoint declared no workspace dependency
	TranscriptBoundaryID string
	ReplayedToolCalls    []string // non-nil once accounted; empty is itself the evidence (nothing replayed)
	ReusedToolCalls      []string // non-nil once accounted
	ConfigModelChanges   []string // non-nil once accounted; empty means no drift across the recovery
	SemanticLossAssessed bool     // the assessment was made (the warning below may still be empty)
	SemanticLossWarning  string
	DurationMS           int64
}

// Complete reports whether the proof carries every §26.12 field a recovery claim requires. The
// list fields must be non-nil (accounted) even when empty — an empty ReplayedToolCalls is the
// evidence that nothing was replayed, but a nil one means the accounting never happened. The
// attempt ids must be present AND distinct (a recovery opens a NEW attempt), and the duration must
// be measured. Checkpoint id and transcript boundary anchor the recovery to durable objects.
func (p RecoveryProof) Complete() bool {
	return p.PreviousAttemptID != "" &&
		p.NewAttemptID != "" &&
		p.PreviousAttemptID != p.NewAttemptID &&
		p.Level != "" &&
		p.CheckpointID != "" &&
		p.TranscriptBoundaryID != "" &&
		p.ReplayedToolCalls != nil &&
		p.ReusedToolCalls != nil &&
		p.ConfigModelChanges != nil &&
		p.SemanticLossAssessed &&
		p.DurationMS > 0
}
