package recovery

import "fmt"

// Level is a recovery ladder rung (spec §26.3). It is journaled on attempt.recovering.v1 and
// carried in the RecoveryProof, so the chosen rung is always VISIBLE — a transcript
// reconstruction is never labelled "exact resume".
type Level string

const (
	// LevelExact continues the original process: its current fenced lease is still alive, so the
	// new attempt stands down and never touches the checkpoint (spec §26.3 rung 1). In this stdio
	// topology "exact" is a lease-liveness confirmation, not a socket reconnect (T4 ceiling).
	LevelExact Level = "exact"
	// LevelCompatibleCheckpoint restores a portable checkpoint into a fresh process via run.restore
	// (spec §26.3 rung 2): every §26.4 compatibility condition passed.
	LevelCompatibleCheckpoint Level = "compatible_checkpoint"
	// LevelTranscriptReconstruction rebuilds from canonical messages + committed step results (spec
	// §26.3 rung 3, the E08 LookupModelResult replay). NEVER called an exact resume.
	LevelTranscriptReconstruction Level = "transcript_reconstruction"
	// LevelExplicitFailure fails the run with a typed reason (spec §26.3 rung 4): no live process, no
	// compatible checkpoint, and reconstruction is impossible or forbidden — never a silent drop.
	LevelExplicitFailure Level = "explicit_failure"
)

// Candidate is the durable checkpoint the ladder weighs plus the integrity result of retrieving
// its bytes. Present is false when the run has no checkpoint at all. ComputedChecksum is the
// sha256 of the bytes actually fetched from the object store; it stays empty when the bytes were
// not (or could not be) retrieved, which fails the checksum condition rather than passing it.
type Candidate struct {
	Present             bool
	Format              string
	FormatVersion       int
	RecordedChecksum    string
	ComputedChecksum    string
	ConfigSnapshotHash  string
	ProtocolVersion     string
	TranscriptSequence  int64
	WorkspaceSnapshotID string // "" declares NO workspace dependency (§26.4)
	WorkspaceRestorable bool   // meaningful only when a snapshot id is present
}

// Target is the recovery context the candidate is weighed against: whether the original attempt's
// lease is still alive, the engine we would restore INTO, the current transcript boundary, and the
// reconstruction policy.
type Target struct {
	OriginalLeaseAlive      bool
	SupportedFormats        []string // engine.ready.checkpoint_formats, e.g. ["reference-kernel/1"]
	ConfigSnapshotHash      string
	ProtocolVersion         string
	JournalSequence         int64
	TranscriptAvailable     bool // committed steps exist to reconstruct from
	ReconstructionForbidden bool // policy knob; default false = reconstruction allowed
}

// Decision is the chosen rung plus, for a non-exact fallback, the compatibility conditions that
// failed — the reasons that ride checkpoint.rejected.v1 and the RecoveryProof (§26.4, §26.12).
type Decision struct {
	Level    Level
	Failures []string
}

// Decide walks the recovery ladder in §26.3 order and returns the rung to take. It is PURE: no IO,
// no clock — the caller resolves lease liveness, fetches + checksums the bytes, and reads the
// current config/journal, then hands those facts here. The order is load-bearing: exact is decided
// BEFORE the checkpoint is read (rung 1 never touches the bytes), then compatibility, then
// reconstruction-or-failure.
func Decide(c Candidate, t Target) Decision {
	if t.OriginalLeaseAlive {
		return Decision{Level: LevelExact}
	}
	if c.Present {
		if fails := compatibilityFailures(c, t); len(fails) == 0 {
			return Decision{Level: LevelCompatibleCheckpoint}
		} else {
			return reconstructOrFail(t, fails)
		}
	}
	return reconstructOrFail(t, []string{"no_checkpoint"})
}

// compatibilityFailures evaluates the §26.4 seven-condition compatibility decision, returning every
// condition that failed (empty = compatible). Each failure is a stable, typed reason so a rejected
// checkpoint records WHY. The workspace condition is vacuously satisfied when the checkpoint
// declares no workspace dependency (T4: snapshot RESTORE is T6).
func compatibilityFailures(c Candidate, t Target) []string {
	var f []string
	formatID := fmt.Sprintf("%s/%d", c.Format, c.FormatVersion)
	switch {
	case !containsFormatName(t.SupportedFormats, c.Format):
		f = append(f, "format_unsupported")
	case !contains(t.SupportedFormats, formatID):
		f = append(f, "format_version_unsupported")
	}
	if c.ComputedChecksum == "" || c.ComputedChecksum != c.RecordedChecksum {
		f = append(f, "checksum_mismatch")
	}
	if c.ConfigSnapshotHash != t.ConfigSnapshotHash {
		f = append(f, "config_changed")
	}
	if c.ProtocolVersion != t.ProtocolVersion {
		f = append(f, "protocol_incompatible")
	}
	if c.TranscriptSequence > t.JournalSequence {
		f = append(f, "boundary_inconsistent")
	}
	if c.WorkspaceSnapshotID != "" && !c.WorkspaceRestorable {
		f = append(f, "workspace_unrestorable")
	}
	return f
}

// reconstructOrFail is the shared tail for an absent or incompatible checkpoint: transcript
// reconstruction if policy allows it and a transcript exists, otherwise an explicit failure with a
// typed reason (spec §26.3 rungs 3-4).
func reconstructOrFail(t Target, reasons []string) Decision {
	if t.ReconstructionForbidden {
		return Decision{Level: LevelExplicitFailure, Failures: reasons}
	}
	if !t.TranscriptAvailable {
		return Decision{Level: LevelExplicitFailure, Failures: append(reasons, "no_transcript")}
	}
	return Decision{Level: LevelTranscriptReconstruction, Failures: reasons}
}

// containsFormatName reports whether any supported "name/version" id shares the candidate's format
// name — a known format whose version may still be unsupported.
func containsFormatName(supported []string, name string) bool {
	prefix := name + "/"
	for _, s := range supported {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
