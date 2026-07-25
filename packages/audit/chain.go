// Package audit is the SEC-103 audit-integrity chain (E18 T7; it closes the E13-H "audit integrity
// linkage" debt). It is a rolling SHA-256 over the `events` journal that `palai audit verify`
// RECOMPUTES FROM THE ROWS and compares against a checkpoint signed with the release signer.
//
// WHY THE CHECKPOINT LIVES OUTSIDE THE DATABASE (plan §1, and the reason this task ships no
// migration): an anchor stored in the mutable store it is meant to protect proves nothing — whoever
// can edit a payload row can edit the anchor row in the same transaction. The checkpoint is a signed
// FILE; the signature is the E14 T5 openssl P-256 signer, and the public key must arrive OUT OF BAND
// (checkpoint.go enforces that, the same fail-closed posture as scripts/release/provenance-verify.sh).
//
// WHAT A CHECKPOINT ANCHORS: a PREFIX. Per session it records (count, max_seq, head) at the moment it
// was cut; the journal keeps growing afterwards, so rows above a session's max_seq — and sessions the
// checkpoint never saw — are UNANCHORED and reported as such, never silently blessed. That is the
// honest ceiling of the whole mechanism: checkpoint cadence is operator policy (plan §T7), and
// everything written since the last checkpoint is only as protected as the next one makes it.
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
)

// Algorithm names the chain construction a checkpoint was cut with. A verifier that meets a
// checkpoint claiming any other algorithm REFUSES rather than comparing incomparable hashes — a
// future construction gets a new name, never a silent redefinition of this one.
const Algorithm = "palai-audit-chain-sha256-v1"

// ChainedColumns is the `events` column set the row digest covers, in digest order. It is exported so
// a guard test can assert it equals the table's ACTUAL column set: a migration that adds a column
// would otherwise leave that column outside the chain, i.e. tamperable without a tamper alert.
var ChainedColumns = []string{
	"id", "organization_id", "project_id", "session_id", "response_id",
	"seq", "journal_id", "type", "payload", "created_at",
}

// Row is one `events` row in chain form. Payload is the jsonb TEXT rendering and CreatedAt the UTC
// microsecond rendering — both produced by the SQL in RowsQuery so the digest does not depend on a
// client's timezone or on Go's JSON key order.
//
// ponytail: the jsonb text form is canonical WITHIN a Postgres major version (keys sorted by length
// then bytes). A major-version upgrade that re-renders jsonb differently invalidates old checkpoints;
// re-cut a checkpoint after such an upgrade, exactly as after a schema change.
type Row struct {
	ID             string
	OrganizationID string
	ProjectID      string
	SessionID      string
	ResponseID     string
	HasResponseID  bool
	Seq            int64
	JournalID      int64
	Type           string
	Payload        string
	CreatedAt      string
}

// digest hashes one row over length-prefixed fields, so no field's bytes can be shifted into a
// neighbour's (a plain concatenation lets "ab"+"c" and "a"+"bc" collide). A NULL response_id is a
// presence byte, distinct from the empty string.
func (r Row) digest() [32]byte {
	h := sha256.New()
	field := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(s))
	}
	field(r.ID)
	field(r.OrganizationID)
	field(r.ProjectID)
	field(r.SessionID)
	if r.HasResponseID {
		_, _ = h.Write([]byte{1})
		field(r.ResponseID)
	} else {
		_, _ = h.Write([]byte{0})
	}
	field(strconv.FormatInt(r.Seq, 10))
	field(strconv.FormatInt(r.JournalID, 10))
	field(r.Type)
	field(r.Payload)
	field(r.CreatedAt)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Head folds rows into the rolling chain head: h(0) is 32 zero bytes, h(i) = sha256(h(i-1) || digest(row i)).
// Rows are chained in canonical order (session_id, then seq) regardless of the order given, so a
// verifier and a checkpointer that read the same rows always agree.
func Head(rows []Row) string {
	ordered := make([]Row, len(rows))
	copy(ordered, rows)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].SessionID != ordered[j].SessionID {
			return ordered[i].SessionID < ordered[j].SessionID
		}
		return ordered[i].Seq < ordered[j].Seq
	})
	acc := make([]byte, 32)
	for _, r := range ordered {
		d := r.digest()
		next := sha256.Sum256(append(acc, d[:]...))
		acc = next[:]
	}
	return "sha256:" + hex.EncodeToString(acc)
}

// SessionAnchor is one session's anchored prefix: how many rows it had, the highest seq among them,
// and the chain head over exactly those rows.
type SessionAnchor struct {
	SessionID string `json:"session_id"`
	Count     int    `json:"count"`
	MaxSeq    int64  `json:"max_seq"`
	Head      string `json:"head"`
}

// Summarize folds rows into the per-session anchors plus the global head over all of them. It is what
// a checkpoint records and what a verify run recomputes; the two are compared field by field.
func Summarize(rows []Row) (head string, sessions []SessionAnchor, count int) {
	bySession := map[string][]Row{}
	for _, r := range rows {
		bySession[r.SessionID] = append(bySession[r.SessionID], r)
	}
	ids := make([]string, 0, len(bySession))
	for id := range bySession {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rs := bySession[id]
		var maxSeq int64
		for _, r := range rs {
			if r.Seq > maxSeq {
				maxSeq = r.Seq
			}
		}
		sessions = append(sessions, SessionAnchor{SessionID: id, Count: len(rs), MaxSeq: maxSeq, Head: Head(rs)})
	}
	return Head(rows), sessions, len(rows)
}
