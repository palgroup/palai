// Package dr holds the E15 T5 disaster-recovery drill harness and its measurement/report logic.
//
// The measurement code in THIS file is pure (no Docker, no build tag), so it rides `go test ./...`;
// the live two-stack drills live in drills_test.go behind `//go:build uat`. The split matters for
// the anti-fabrication anchor (plan §2): every MEASURED value (RPO/RTO) is COMPUTED from raw
// timestamps by ComputeRPO/ComputeRTO here, stored in the evidence artifact BESIDE those raw
// timestamps, and re-derivable — Verify (and, downstream, E15 T6's DrillProof verifier) recomputes
// from the raw data and FAILs on a value the timestamps do not support. A hand-written or hard-coded
// RPO/RTO cannot survive that recompute.
//
// Honest ceiling (plan §6, §T5): these drills prove recovery on the LOCAL same-host two-stack —
// "primary loss" is container + volume destruction on ONE Docker Desktop host. A real instance/zone
// loss and a separate-physical-host restore are the operator leg (§6, incl. E14 leg 2); DR-003
// regional failover is the SaaS plan; the KMS key ceremony is E13-H. The report names all of this.
package dr

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// tsLayout is the raw-timestamp encoding in the evidence artifact — RFC3339 with nanoseconds, so a
// verifier re-parses to the exact instant the drill recorded and the recompute reproduces the value.
const tsLayout = time.RFC3339Nano

// DrillEvidence is the machine-readable DR-drill artifact. Every measured value lies here beside the
// RAW timestamps it was computed from (Measure), so the recompute-and-fail-on-mismatch gate can run.
type DrillEvidence struct {
	GeneratedAt string           `json:"generated_at"`
	GitSHA      string           `json:"git_sha"`
	Seam        string           `json:"seam"` // the honest ceiling this run proves (local same-host two-stack)
	Drills      []DrillResult    `json:"drills"`
	Targets     PublishedTargets `json:"published_targets"`
	Findings    []Finding        `json:"findings"`
}

// DrillResult is one drill's outcome. Measure is nil for detection-only drills (DR-004 object
// corruption, DR-005 key recovery) that carry no RPO/RTO — they prove fail-closed detection, not a
// timed recovery.
type DrillResult struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Passed  bool     `json:"passed"`
	Detail  string   `json:"detail"`
	Measure *Measure `json:"measure,omitempty"`
}

// Measure carries the RAW timestamps plus the values COMPUTED from them. The invariant Verify
// enforces: RPOSeconds == ComputeRPO(LastMarkerWrittenAt, LastMarkerInBackupAt) and RTOSeconds ==
// ComputeRTO(DisasterAt, RecoveredAt). The timestamps are the source of truth; the seconds are a
// derived convenience a verifier must be able to reproduce.
//
//   - RPO (data-loss window) uses the DATABASE clock: LastMarkerWrittenAt is the last steady-traffic
//     marker committed before the disaster (captured by the writer as it wrote); LastMarkerInBackupAt
//     is the newest marker the recovery's backup actually restored. Same clock -> the difference is
//     the genuine window of writes the backup did not capture.
//   - RTO (recovery wall-clock) uses the HOST clock: DisasterAt is when the destruction completed and
//     RecoveredAt is when the stack was healthy AND run-capable again (a run reached terminal).
type Measure struct {
	LastMarkerWrittenAt  string  `json:"last_marker_written_at"`
	LastMarkerInBackupAt string  `json:"last_marker_in_backup_at"`
	RPOSeconds           float64 `json:"rpo_seconds"`
	DisasterAt           string  `json:"disaster_at"`
	RecoveredAt          string  `json:"recovered_at"`
	RTOSeconds           float64 `json:"rto_seconds"`
}

// PublishedTargets is the self-host's PUBLISHED achievable DR posture (plan §55.5). These are NOT
// inherited from the SaaS product: self-host is single-node, so RPO is bounded by the backup cadence
// and RTO by the scripted restore, and Basis says exactly why.
type PublishedTargets struct {
	RPO   string `json:"rpo"`
	RTO   string `json:"rto"`
	Basis string `json:"basis"`
}

// Finding is one §55.4 DR finding + its remediation.
type Finding struct {
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation"`
}

// round3 rounds to milliseconds so a stored value and a recomputed value compare exactly (both go
// through this) while staying precise enough for a wall-clock RTO / a sub-second RPO.
func round3(s float64) float64 { return math.Round(s*1000) / 1000 }

// ComputeRPO is the marker-loss window in seconds: last committed marker minus last marker in the
// backup the recovery restored from. Negative would mean the backup captured a write newer than the
// last one committed before the disaster (impossible in a real drill) — the primitive reports it
// honestly and Verify flags it.
func ComputeRPO(lastWritten, lastInBackup time.Time) float64 {
	return round3(lastWritten.Sub(lastInBackup).Seconds())
}

// ComputeRTO is the recovery wall-clock in seconds: healthy-and-run-capable instant minus disaster
// instant.
func ComputeRTO(disaster, recovered time.Time) float64 {
	return round3(recovered.Sub(disaster).Seconds())
}

// NewMeasure builds a Measure from the four raw instants — the SINGLE place RPO/RTO are computed, so
// the artifact and the rendered report are consistent by construction and Verify's recompute matches.
func NewMeasure(lastWritten, lastInBackup, disaster, recovered time.Time) Measure {
	return Measure{
		LastMarkerWrittenAt:  lastWritten.UTC().Format(tsLayout),
		LastMarkerInBackupAt: lastInBackup.UTC().Format(tsLayout),
		RPOSeconds:           ComputeRPO(lastWritten, lastInBackup),
		DisasterAt:           disaster.UTC().Format(tsLayout),
		RecoveredAt:          recovered.UTC().Format(tsLayout),
		RTOSeconds:           ComputeRTO(disaster, recovered),
	}
}

// Verify recomputes every measured RPO/RTO from its raw timestamps and returns a finding for each
// value the raw data does not support (or any unparseable/negative-window timestamp). An empty slice
// is a clean, non-fabricated artifact. This is the anti-fabrication anchor E15 T6's DrillProof mirrors.
func Verify(ev DrillEvidence) []error {
	const eps = 1e-6
	var errs []error
	for _, d := range ev.Drills {
		m := d.Measure
		if m == nil {
			continue
		}
		lw, err := time.Parse(tsLayout, m.LastMarkerWrittenAt)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: last_marker_written_at %q unparseable: %w", d.ID, m.LastMarkerWrittenAt, err))
			continue
		}
		lb, err := time.Parse(tsLayout, m.LastMarkerInBackupAt)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: last_marker_in_backup_at %q unparseable: %w", d.ID, m.LastMarkerInBackupAt, err))
			continue
		}
		da, err := time.Parse(tsLayout, m.DisasterAt)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: disaster_at %q unparseable: %w", d.ID, m.DisasterAt, err))
			continue
		}
		ra, err := time.Parse(tsLayout, m.RecoveredAt)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: recovered_at %q unparseable: %w", d.ID, m.RecoveredAt, err))
			continue
		}
		if wantRPO := ComputeRPO(lw, lb); math.Abs(wantRPO-m.RPOSeconds) > eps {
			errs = append(errs, fmt.Errorf("%s: rpo_seconds=%.3f is not derivable from the raw markers (recompute=%.3f) — a fabricated measurement", d.ID, m.RPOSeconds, wantRPO))
		}
		if wantRTO := ComputeRTO(da, ra); math.Abs(wantRTO-m.RTOSeconds) > eps {
			errs = append(errs, fmt.Errorf("%s: rto_seconds=%.3f is not derivable from disaster/recovered (recompute=%.3f) — a fabricated measurement", d.ID, m.RTOSeconds, wantRTO))
		}
		if m.RPOSeconds < 0 {
			errs = append(errs, fmt.Errorf("%s: rpo_seconds=%.3f is negative — the backup cannot hold a marker newer than the last committed one", d.ID, m.RPOSeconds))
		}
		if m.RTOSeconds < 0 {
			errs = append(errs, fmt.Errorf("%s: rto_seconds=%.3f is negative — recovery cannot precede the disaster", d.ID, m.RTOSeconds))
		}
	}
	return errs
}

// DefaultTargets is the PUBLISHED self-host DR posture (plan §55.5) — single-node, backup-cadence
// bound, explicitly NOT the SaaS targets.
func DefaultTargets() PublishedTargets {
	return PublishedTargets{
		RPO: "<= one backup interval (with the shipped hourly `deploy/systemd` backup timer, <= 1h; the drill floor below shows the data-loss window is only the writes since the last backup)",
		RTO: "<= 15 min for a single-node restore-to-fresh-target on comparable hardware (the drill floor below is the scripted-recovery wall-clock; add human detection/paging for the operator SLO)",
		Basis: "self-host is a single Postgres + a single object store on one node: there is no synchronous replica, so recovery is restore-from-backup and the achievable RPO is bounded by how often `palai backup` runs, not by a SaaS replication SLA. These targets are PUBLISHED for the self-host tier and are NOT inherited from the managed SaaS product.",
	}
}

// DefaultFindings is the §55.4 findings/remediation set. The single-node RPO finding is derived from
// the measured drill floor so it stays honest run-to-run.
func DefaultFindings(measuredRPO float64) []Finding {
	return []Finding{
		{
			Severity:    "expected",
			Title:       "RPO is bounded by backup cadence, not by replication",
			Detail:      fmt.Sprintf("the primary-loss drill lost only the %.3fs of markers written between the last backup and the disaster, because the backup was taken immediately before the failure; in production the real window is everything written since the last scheduled backup ran.", measuredRPO),
			Remediation: "shorten the `deploy/systemd/palai-backup.timer` interval to cap the window; to drive RPO toward zero, add Postgres WAL archiving or a streaming replica — both are the operator/E18 leg (out of scope for this single-node drill).",
		},
		{
			Severity:    "expected",
			Title:       "RTO includes no human detection/decision time",
			Detail:      "the measured RTO is the SCRIPTED recovery wall-clock (destroy -> fresh pg -> restore -> healthy -> run-capable). It does not include the time for a human to notice the outage and start the runbook.",
			Remediation: "publish an operator SLO = measured RTO + a detection budget; wire the shipped `PalaiDown`/`palai_db_up` alert (deploy/observability) to page so detection is bounded.",
		},
		{
			Severity:    "ceiling",
			Title:       "same-host drill, not a second-site failover",
			Detail:      "primary loss here is container + volume destruction on ONE Docker Desktop host; the object store and DB come back on the same machine. A real instance/zone loss, a separate-physical-host restore (E14 operator leg 2), and DR-003 cross-region failover are NOT proven here.",
			Remediation: "run the same harness against a separate physical host / cloud VM (the parametric operator leg, plan §6); DR-003 regional failover is the SaaS plan.",
		},
		{
			Severity:    "ceiling",
			Title:       "master key is a file, not KMS",
			Detail:      "the key-recovery drill (DR-005) proves the FILE master-key seam is fail-closed and recoverable from an escrow copy. A KMS-backed key + lease ceremony (SEC-001/003) is out of scope.",
			Remediation: "the KMS ceremony is the E13-H hardening tranche; until then, keep the master key in escrow (a sealed, offsite copy) — the drill proves a wrong/absent key fails closed at `restore verify`.",
		},
	}
}

// RenderReport machine-generates docs/operations/dr-report.md from an evidence artifact: a measured
// RPO/RTO table (from the raw timestamps), the published self-host targets, and the findings +
// remediation. Nothing here is hand-written per-run — it is a projection of the verified evidence.
func RenderReport(ev DrillEvidence) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("# Palai Self-Host DR Drill Report\n\n")
	p("> MACHINE-GENERATED by the E15 T5 DR drill harness (`tests/uat/dr`). Do not hand-edit: every\n")
	p("> RPO/RTO below is recomputed from the raw timestamps in the evidence artifact by `dr.Verify`\n")
	p("> and again by the E15 T6 `DrillProof` verifier — a hand-written value fails the gate.\n\n")
	p("- Generated: `%s`\n", ev.GeneratedAt)
	p("- Commit: `%s`\n", ev.GitSHA)
	p("- Proof seam: **%s**\n\n", ev.Seam)

	p("## Measured RPO / RTO (from raw timestamps)\n\n")
	p("| Drill | Scenario | Result | Measured RPO | Measured RTO |\n")
	p("|---|---|---|---|---|\n")
	for _, d := range ev.Drills {
		result := "PASS"
		if !d.Passed {
			result = "**FAIL**"
		}
		rpo, rto := "—", "—"
		if d.Measure != nil {
			rpo = fmt.Sprintf("%.3fs", d.Measure.RPOSeconds)
			rto = fmt.Sprintf("%.3fs", d.Measure.RTOSeconds)
		}
		p("| %s | %s | %s | %s | %s |\n", d.ID, d.Name, result, rpo, rto)
	}
	p("\n")

	// The raw timestamps each measured row was computed from — so a reader (and the T6 verifier) can
	// recompute by hand: RPO = last_marker_written_at - last_marker_in_backup_at; RTO = recovered_at
	// - disaster_at.
	for _, d := range ev.Drills {
		if d.Measure == nil {
			continue
		}
		m := d.Measure
		p("### %s raw timestamps\n\n", d.ID)
		p("- last marker committed before disaster: `%s`\n", m.LastMarkerWrittenAt)
		p("- last marker present in the restored backup: `%s`\n", m.LastMarkerInBackupAt)
		p("- RPO = committed - in-backup = **%.3fs**\n", m.RPOSeconds)
		p("- disaster (destruction complete): `%s`\n", m.DisasterAt)
		p("- recovered (healthy + run-capable): `%s`\n", m.RecoveredAt)
		p("- RTO = recovered - disaster = **%.3fs**\n\n", m.RTOSeconds)
	}

	p("## Per-drill detail\n\n")
	for _, d := range ev.Drills {
		p("- **%s (%s)** — %s\n", d.ID, d.Name, d.Detail)
	}
	p("\n")

	p("## Published self-host targets (§55.5 — NOT inherited from SaaS)\n\n")
	p("- **RPO target:** %s\n", ev.Targets.RPO)
	p("- **RTO target:** %s\n", ev.Targets.RTO)
	p("- **Basis:** %s\n\n", ev.Targets.Basis)

	p("## Findings & Remediation (§55.4)\n\n")
	// Stable ordering so a regenerated report has a minimal diff.
	findings := append([]Finding(nil), ev.Findings...)
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Title < findings[j].Title })
	for _, f := range findings {
		p("### [%s] %s\n\n", f.Severity, f.Title)
		p("- **Finding:** %s\n", f.Detail)
		p("- **Remediation:** %s\n\n", f.Remediation)
	}

	p("## Honest ceiling\n\n")
	p("This report proves recovery on the **local same-host two-stack** only. Primary loss is container\n")
	p("+ volume destruction on ONE Docker Desktop host; a real instance/zone loss and a separate-physical-\n")
	p("host restore are the operator leg (plan §6, incl. E14 leg 2). **DR-003 regional failover is the SaaS\n")
	p("plan**, and the KMS key ceremony is E13-H. The harness is parametric on `PALAI_HOME` + the compose\n")
	p("files, so an operator points it at a separate host unchanged (`docs/operations/dr-drills.md`).\n")
	return b.String()
}
