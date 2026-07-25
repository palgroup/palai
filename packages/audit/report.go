package audit

import (
	"fmt"
	"sort"
)

// Alert kinds. These are the TYPED surface a caller asserts on — never a substring of prose, so a
// failing run and a passing run can never be told apart by grepping the same word (the E15 T6 lesson).
const (
	// AlertGap: rows the checkpoint anchored are MISSING — an interior hole in a session's seq,
	// a truncated tail, or a whole anchored session gone.
	AlertGap = "gap"
	// AlertTamper: the anchored rows are all still there (count, max_seq and contiguity match the
	// checkpoint exactly) but their BYTES no longer chain to the anchored head.
	AlertTamper = "tamper"
	// AlertSignature: the checkpoint itself is not trustworthy — bad/absent signature, untrusted key
	// resolution, or an unreadable envelope. Raised before any row is even read.
	AlertSignature = "signature"
	// AlertMalformed: the checkpoint parses but does not describe a chain this verifier can compare
	// (wrong algorithm/version). Refused rather than compared.
	AlertMalformed = "malformed"
)

// Alert is one typed integrity finding. Detail is for a human; Kind is what a test or an alerting
// rule matches on.
type Alert struct {
	Kind      string `json:"kind"`
	SessionID string `json:"session_id,omitempty"`
	Detail    string `json:"detail"`
	Want      string `json:"want,omitempty"`
	Got       string `json:"got,omitempty"`
}

// Report is the whole outcome of one verify run. OK is false whenever Alerts is non-empty, and the
// command exits non-zero on exactly that condition: an integrity alert is never a log line someone
// has to notice.
type Report struct {
	Algorithm      string  `json:"algorithm"`
	CheckpointPath string  `json:"checkpoint"`
	CheckpointHead string  `json:"checkpoint_head"`
	RecomputedHead string  `json:"recomputed_head"`
	AnchoredRows   int     `json:"anchored_rows"`
	UnanchoredRows int     `json:"unanchored_rows"`
	Signature      string  `json:"signature"`
	Alerts         []Alert `json:"alerts"`
	OK             bool    `json:"ok"`
}

// Has reports whether the report carries at least one alert of kind.
func (r Report) Has(kind string) bool {
	for _, a := range r.Alerts {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

// Compare recomputes the chain from rows and measures it against what the checkpoint anchored.
//
// Only the ANCHORED PREFIX is compared: per checkpointed session, the rows at or below that session's
// max_seq. Rows above it, and sessions the checkpoint never saw, are counted as unanchored and
// reported — they are newer than the anchor, so no verdict about them is possible or claimed.
//
// gap vs tamper: a session whose anchored seq set is intact (same count, same max_seq, contiguous
// 1..max) but whose head differs is a TAMPER — the only thing that can have changed is bytes. A
// session whose seq set is broken is a GAP, and no tamper is additionally claimed for it: the head
// difference is already fully explained by the missing rows, and a tamper alert that co-fires on
// every gap would make the byte-level arm vacuous.
func Compare(cp Checkpoint, rows []Row) Report {
	rep := Report{Algorithm: cp.Algorithm, CheckpointHead: cp.Head}

	if cp.Version != CheckpointVersion || cp.Algorithm != Algorithm {
		rep.Alerts = append(rep.Alerts, Alert{
			Kind: AlertMalformed,
			Detail: fmt.Sprintf("checkpoint declares version %d / algorithm %q; this verifier only compares version %d / %q",
				cp.Version, cp.Algorithm, CheckpointVersion, Algorithm),
			Want: fmt.Sprintf("%d/%s", CheckpointVersion, Algorithm),
			Got:  fmt.Sprintf("%d/%s", cp.Version, cp.Algorithm),
		})
		return rep
	}

	bySession := map[string][]Row{}
	for _, r := range rows {
		bySession[r.SessionID] = append(bySession[r.SessionID], r)
	}

	anchors := make([]SessionAnchor, len(cp.Sessions))
	copy(anchors, cp.Sessions)
	sort.Slice(anchors, func(i, j int) bool { return anchors[i].SessionID < anchors[j].SessionID })

	var anchored []Row
	for _, a := range anchors {
		var prefix []Row
		seen := map[int64]bool{}
		for _, r := range bySession[a.SessionID] {
			if r.Seq <= a.MaxSeq {
				prefix = append(prefix, r)
				seen[r.Seq] = true
			}
		}
		anchored = append(anchored, prefix...)

		if missing := missingSeqs(seen, a.MaxSeq); len(missing) > 0 || len(prefix) != a.Count {
			rep.Alerts = append(rep.Alerts, Alert{
				Kind:      AlertGap,
				SessionID: a.SessionID,
				Detail: fmt.Sprintf("the checkpoint anchored %d row(s) up to seq %d; %d are present, missing seq %v",
					a.Count, a.MaxSeq, len(prefix), missing),
				Want: fmt.Sprintf("%d rows", a.Count),
				Got:  fmt.Sprintf("%d rows", len(prefix)),
			})
			continue
		}
		if got := Head(prefix); got != a.Head {
			rep.Alerts = append(rep.Alerts, Alert{
				Kind:      AlertTamper,
				SessionID: a.SessionID,
				Detail: fmt.Sprintf("every anchored row is present (%d rows up to seq %d) but their bytes no longer chain to the anchored head",
					a.Count, a.MaxSeq),
				Want: a.Head,
				Got:  got,
			})
		}
	}

	rep.AnchoredRows = len(anchored)
	rep.UnanchoredRows = len(rows) - len(anchored)
	rep.RecomputedHead = Head(anchored)
	if rep.RecomputedHead != cp.Head && len(rep.Alerts) == 0 {
		// Belt and braces: per-session comparison should already have caught anything that moves the
		// global head. If it did not, the checkpoint's own head disagrees with its own session anchors.
		rep.Alerts = append(rep.Alerts, Alert{
			Kind:   AlertTamper,
			Detail: "the recomputed head over the anchored prefix differs from the checkpoint head while every session anchor matched — the checkpoint is internally inconsistent",
			Want:   cp.Head,
			Got:    rep.RecomputedHead,
		})
	}
	rep.OK = len(rep.Alerts) == 0
	return rep
}

// missingSeqs lists the 1..maxSeq values absent from seen, bounded so a wholly-deleted session
// reports a readable head rather than thousands of integers.
func missingSeqs(seen map[int64]bool, maxSeq int64) []int64 {
	var out []int64
	for s := int64(1); s <= maxSeq; s++ {
		if !seen[s] {
			out = append(out, s)
			if len(out) == 16 {
				return out
			}
		}
	}
	return out
}
