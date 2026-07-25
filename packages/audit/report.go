package audit

import (
	"fmt"
	"sort"
	"time"
)

// Alert kinds. These are the TYPED surface a caller asserts on — never a substring of prose, so a
// failing run and a passing run can never be told apart by grepping the same word (the E15 T6 lesson).
const (
	// AlertGap: rows the checkpoint anchored are MISSING — a session holds fewer anchored rows than
	// were anchored, tops out below the anchored seq, or is gone entirely.
	AlertGap = "gap"
	// AlertTamper: the anchored rows are all still there (same count, same top seq) but their BYTES
	// no longer chain to the anchored head.
	AlertTamper = "tamper"
	// AlertSignature: the checkpoint itself is not trustworthy — bad/absent signature, untrusted key
	// resolution, or an unreadable envelope. Raised before any row is even read.
	AlertSignature = "signature"
	// AlertMalformed: the checkpoint parses but does not describe a chain this verifier can compare
	// (wrong algorithm/version). Refused rather than compared.
	AlertMalformed = "malformed"
	// AlertStale: the checkpoint is validly signed and its own prefix is intact, but it is NOT A
	// CURRENT ANCHOR. This is the rollback arm: swap in an older signed copy (or restore one out of a
	// stale backup) and the signature still verifies, the whole journal since becomes "unanchored",
	// and every edit made in the meantime raises nothing at all. Only an operator-declared freshness
	// window (--not-older-than) or coverage floor (--min-anchored) can tell old from fresh; Compare
	// alone cannot, because an old checkpoint is indistinguishable from a correct one that simply
	// predates a lot of growth.
	AlertStale = "stale"
)

// Ceilings is what this verifier does NOT cover, carried in every report — the JSON one included, so
// the T10 evidence consumer reads the same honest limits a human does rather than only the operator
// who happened to run it on a terminal.
var Ceilings = []string{
	"This chains the `events` session journal, NOT `audit_events` (§50.3, protected by a REVOKE of UPDATE/DELETE instead). Neither control substitutes for the other.",
	"A checkpoint anchors a PREFIX. Everything written since the last cut is unanchored and only as protected as the next cut makes it; cadence is operator policy.",
	"ROLLBACK: an OLD checkpoint is still validly signed. Its own prefix verifies green and everything after it is merely 'unanchored', so swapping in a stale signed copy hides every later edit. Pass --not-older-than (and --min-anchored) to make that a `stale` alert, and keep the checkpoint somewhere a restore of the database cannot roll back with it.",
	"AN AUTHORISED RETENTION PURGE LOOKS EXACTLY LIKE TAMPERING: §22.2 scrub_events UPDATEs an anchored row's payload to {\"purged\": true} — same rows, same seq, different bytes — which is precisely the `tamper` signature. RE-CUT THE CHECKPOINT AFTER A PURGE; until you do, this alert cannot tell the reaper from an attacker.",
	"Continuous live verification wired to alerting is a plan §6 operator leg. This command is the mechanism, not a running watchdog.",
}

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
	Algorithm string `json:"algorithm"`
	// CheckpointGeneratedAt and CheckpointAge are the checkpoint's OWN age, read back out of the file
	// and printed. Without them a 90-day-old signed anchor and one cut a minute ago are the same
	// report, which is the whole rollback hole (AlertStale).
	CheckpointGeneratedAt string   `json:"checkpoint_generated_at"`
	CheckpointAge         string   `json:"checkpoint_age,omitempty"`
	CheckpointPath        string   `json:"checkpoint"`
	CheckpointHead        string   `json:"checkpoint_head"`
	RecomputedHead        string   `json:"recomputed_head"`
	AnchoredRows          int      `json:"anchored_rows"`
	UnanchoredRows        int      `json:"unanchored_rows"`
	Signature             string   `json:"signature"`
	Ceiling               []string `json:"ceiling"`
	Alerts                []Alert  `json:"alerts"`
	OK                    bool     `json:"ok"`
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
// gap vs tamper: a session that still holds exactly as many anchored rows, up to exactly the same
// seq, but whose head differs is a TAMPER — the only thing that can have changed is bytes. A session
// that has FEWER anchored rows, or a lower top seq, is a GAP, and no tamper is additionally claimed
// for it: the head difference is already fully explained by the missing rows, and a tamper alert that
// co-fired on every gap would make the byte-level arm vacuous.
//
// The gap rule is deliberately NOT "seq 1..max must be contiguous". `events.seq` is assigned by the
// APPLICATION, so an insert that is rolled back (or an id claimed by a run that never got to write)
// burns a number and leaves a permanent hole — contiguity would then cry gap over an ordinary aborted
// transaction. What is anchored is the seq set as it stood when the checkpoint was cut; losing any of
// it shows up as a smaller count or a lower top seq.
//
// The §22.2 RETENTION PURGE IS NOT A SOURCE OF HOLES, and the earlier claim here that it was is
// wrong: `scrub_events` (storage/queries/responses.sql) UPDATEs the payload to {"purged": true} — it
// does not DELETE. There is no `DELETE FROM events` in production code at all. So a purge leaves the
// same rows at the same seq with DIFFERENT BYTES, which is exactly the tamper signature, and on a
// stack running PALAI_RETENTION_STORE_FALSE_TTL the reaper raises AlertTamper on routine maintenance.
// That is named in the alert's own Detail and in Ceilings, because the honest answer is operational
// (re-cut the checkpoint after a purge) and an operator who is not told will read the loudest alert
// this tool has as "the reaper did it" — which is also perfect cover for someone who is not the reaper.
//
// ponytail: count+max_seq localizes a gap to a session, not to the individual missing seq. The
// checkpoint would have to carry the anchored seq set (run-length encoded) to name it; add that if an
// operator ever needs the exact row rather than the session.
func Compare(cp Checkpoint, rows []Row) Report {
	rep := Report{Algorithm: cp.Algorithm, CheckpointHead: cp.Head, CheckpointGeneratedAt: cp.GeneratedAt}

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
		var topSeq int64
		for _, r := range bySession[a.SessionID] {
			if r.Seq <= a.MaxSeq {
				prefix = append(prefix, r)
				if r.Seq > topSeq {
					topSeq = r.Seq
				}
			}
		}
		anchored = append(anchored, prefix...)

		if len(prefix) != a.Count || topSeq != a.MaxSeq {
			rep.Alerts = append(rep.Alerts, Alert{
				Kind:      AlertGap,
				SessionID: a.SessionID,
				Detail: fmt.Sprintf("the checkpoint anchored %d row(s) up to seq %d; %d row(s) up to seq %d are present — anchored rows have been removed",
					a.Count, a.MaxSeq, len(prefix), topSeq),
				Want: fmt.Sprintf("%d rows up to seq %d", a.Count, a.MaxSeq),
				Got:  fmt.Sprintf("%d rows up to seq %d", len(prefix), topSeq),
			})
			continue
		}
		if got := Head(prefix); got != a.Head {
			rep.Alerts = append(rep.Alerts, Alert{
				Kind:      AlertTamper,
				SessionID: a.SessionID,
				Detail: fmt.Sprintf("every anchored row is present (%d rows up to seq %d) but their bytes no longer chain to the anchored head. %s",
					a.Count, a.MaxSeq, purgeCaveat),
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

// purgeCaveat rides on every tamper alert. An operator meeting the loudest alert this tool has must
// be told, right there, that an authorised §22.2 purge produces the identical signature — otherwise
// the first true positive gets waved off, and the first purge gets escalated.
const purgeCaveat = "CAVEAT: an AUTHORISED §22.2 retention purge produces this exact signature — " +
	"scrub_events rewrites an anchored payload to {\"purged\": true} without removing the row. " +
	"If a purge ran since the checkpoint was cut, re-cut it (`palai audit checkpoint`) and re-verify; " +
	"if none did, this is an edit nobody in the retention path made."

// Freshness is the ROLLBACK arm, and it is separate from Compare on purpose: Compare can only ever
// say "this checkpoint's own prefix is intact", which an attacker's older-but-validly-signed copy
// satisfies perfectly. Only the operator knows how often they cut, so only they can say what "too
// old" and "too small" mean here.
//
//	maxAge      > 0: the checkpoint must have been generated within it (--not-older-than).
//	minAnchored > 0: it must anchor at least that many rows (--min-anchored), which catches a
//	                 rollback even on a host whose clock the attacker also controls.
//
// Both zero means the operator declared no window, and no staleness verdict is claimed — the report
// still CARRIES the age either way, so the absence of a policy is visible rather than silent.
func Freshness(cp Checkpoint, maxAge time.Duration, minAnchored int, now time.Time) []Alert {
	var alerts []Alert
	if maxAge > 0 {
		generated, err := time.Parse(time.RFC3339Nano, cp.GeneratedAt)
		if err != nil {
			alerts = append(alerts, Alert{
				Kind:   AlertStale,
				Detail: fmt.Sprintf("--not-older-than was requested but the checkpoint's generated_at %q does not parse as RFC3339, so its age cannot be established", cp.GeneratedAt),
			})
		} else if age := now.Sub(generated); age > maxAge {
			alerts = append(alerts, Alert{
				Kind: AlertStale,
				Detail: fmt.Sprintf("the checkpoint was generated %s ago (%s), outside the %s freshness window — a validly signed OLD anchor still verifies its own prefix and reports everything since as merely unanchored, which is what a rollback looks like",
					age.Truncate(time.Second), cp.GeneratedAt, maxAge),
				Want: "not older than " + maxAge.String(),
				Got:  age.Truncate(time.Second).String(),
			})
		}
	}
	if minAnchored > 0 && cp.Count < minAnchored {
		alerts = append(alerts, Alert{
			Kind: AlertStale,
			Detail: fmt.Sprintf("the checkpoint anchors only %d row(s) against a floor of %d — an anchor that covers almost nothing vouches for almost nothing, whatever its signature says",
				cp.Count, minAnchored),
			Want: fmt.Sprintf("at least %d anchored rows", minAnchored),
			Got:  fmt.Sprintf("%d", cp.Count),
		})
	}
	return alerts
}

// Age renders how long ago the checkpoint was cut, for the report. An unparseable timestamp is not an
// error here — Freshness raises that; this only formats what it can.
func Age(cp Checkpoint, now time.Time) string {
	generated, err := time.Parse(time.RFC3339Nano, cp.GeneratedAt)
	if err != nil {
		return ""
	}
	return now.Sub(generated).Truncate(time.Second).String()
}
