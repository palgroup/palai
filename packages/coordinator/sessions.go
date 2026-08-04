package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// derivedNameRunes bounds the label derived from a session's first prompt. A whole prompt is not a
// table cell, and the cut is by RUNE rather than byte: "Gece Doğrulama" is 14 runes and 16 bytes, and
// a byte cut can land inside a rune and produce invalid UTF-8 in a JSON response.
const derivedNameRunes = 80

// SessionView is a session's projection for GET /v1/sessions/{id} and for one row of
// GET /v1/sessions — the SAME shape, so a list row and a detail read cannot disagree. Found is false
// for an unknown or foreign id (404, no existence disclosure).
//
// The four fields past Found are the Sessions screen (E29, migration 000048). Three are aggregates the
// list query computes per page row; only Name is stored. What each one honestly means:
//
//   - Name/NameSource. Name is the label to render; NameSource says where it came from, because
//     "the operator called this Gece Doğrulama" and "we cut the first 80 runes off its first prompt"
//     are different facts and a screen that offers a rename affordance needs to tell them apart.
//     NameSource is "operator", "derived", or "none".
//   - Agents. The DISTINCT agent-profile names the session's RUNS pinned, sorted. Not "the session's
//     agent": no such association exists in this schema (000019 put agent_revision_id on `runs`), so a
//     session may have none, one, or several. A run pinned to a run_template_revision contributes
//     nothing — 000019 states a template must not impersonate an agent identity.
//   - InputTokens/OutputTokens. Summed from usage_ledger, which is the metering ledger and not a
//     provider invoice. It carries a KNOWN gap its own writer documents (usage.go): the tokens of a
//     model step aborted by an interrupt never reach it, because the provider's counts ride a final
//     stream chunk a canceled stream never receives. So this is tokens METERED, and on a session whose
//     steps were interrupted it is a floor, not a total.
//   - FirstActivityAt/LastActivityAt. min(runs.created_at)/max(runs.updated_at) — written by InsertRun
//     and by UpdateRunState, which rewrites updated_at on every run transition. Both are nil for a
//     session that never ran, which is a session with no duration rather than one lasting zero.
//     LastActivityAt on a LIVE run is its last TRANSITION, not now: a running session's elapsed time is
//     the caller's clock minus FirstActivityAt, not this span.
type SessionView struct {
	ID        string
	State     string
	CreatedAt time.Time
	Found     bool
	// AutoApprove is the session's STANDING AUTHORIZATION for its own approvals (E30 T1, migration
	// 000056), and it is on the projection because an operator must be able to SEE from the screen that
	// a session is deciding its own gated calls. A switch whose state is only visible to the code that
	// reads it is a switch nobody can audit from the outside.
	//
	// It is read by BOTH the detail query and the list query. That is deliberate rather than thorough:
	// populating it on one and leaving the other at the zero value would make a list screen quietly
	// report every armed session as unarmed, which is the more dangerous of the two directions.
	AutoApprove AutoApproveView
	Name        string
	NameSource  string
	Agents      []string

	InputTokens  int64
	OutputTokens int64

	FirstActivityAt *time.Time
	LastActivityAt  *time.Time
}

// CreateSession opens a fresh session (spec §9.1 POST /v1/sessions). The id is caller-minted;
// the session starts active. It is the standalone counterpart of admission's implicit session
// creation, deferred from T1. name is the optional operator label and may be empty, in which case the
// projection falls back to the derived one.
func (s *Store) CreateSession(ctx context.Context, tenant Tenant, sessionID, name string) (SessionView, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	if _, err := s.pool.Exec(ctx, storage.Query("InsertSession"), sessionID, tenant.Project, name); err != nil {
		return SessionView{}, fmt.Errorf("insert session: %w", err)
	}
	return s.GetSession(ctx, tenant, sessionID)
}

// RenameSession sets a session's operator label and returns the re-read projection (E29). A session
// created implicitly by admission has no label, so a rename — not a create-time name — is what a
// Sessions screen actually needs; both exist. An unknown or foreign id yields Found=false and writes
// NOTHING, the same no-existence-disclosure contract GetSession has.
//
// The label is deliberately not unique: the reference screen shows several sessions sharing one, so
// this is a name in the human sense. The id remains the identity.
func (s *Store) RenameSession(ctx context.Context, tenant Tenant, sessionID, name string) (SessionView, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var updated string
	err := s.pool.QueryRow(ctx, storage.Query("RenameSession"), sessionID, tenant.Project, name).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionView{}, nil
	}
	if err != nil {
		return SessionView{}, fmt.Errorf("rename session %s: %w", sessionID, err)
	}
	return s.GetSession(ctx, tenant, sessionID)
}

// GetSession reads a session's projection within the tenant scope (spec §9.1 GET). A foreign
// or unknown id yields Found=false, so the caller renders a 404 that leaks no cross-tenant
// existence (§39.2).
func (s *Store) GetSession(ctx context.Context, tenant Tenant, sessionID string) (SessionView, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var (
		v       SessionView
		derived *string
	)
	err := s.pool.QueryRow(ctx, storage.Query("GetSessionInScope"), sessionID, tenant.Project).
		Scan(&v.ID, &v.State, &v.CreatedAt, &v.Name,
			&v.AutoApprove.Tools, &v.AutoApprove.Publications, &v.AutoApprove.SetBy, &v.AutoApprove.SetAt,
			&derived, &v.Agents,
			&v.InputTokens, &v.OutputTokens, &v.FirstActivityAt, &v.LastActivityAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionView{}, nil
	}
	if err != nil {
		return SessionView{}, fmt.Errorf("read session %s: %w", sessionID, err)
	}
	resolveSessionName(&v, derived)
	v.Found = true
	return v, nil
}

// resolveSessionName settles which of the two labels a row renders and says which it chose. The
// operator's wins whenever it is set; otherwise the derived one is normalised and cut; if there is
// neither, the row reports "none" rather than an empty string that could be either.
func resolveSessionName(v *SessionView, derived *string) {
	if v.Name != "" {
		v.NameSource = "operator"
		return
	}
	if derived != nil {
		if cut := displayLabel(*derived); cut != "" {
			v.Name, v.NameSource = cut, "derived"
			return
		}
	}
	v.Name, v.NameSource = "", "none"
}

// displayLabel folds a prompt into one line short enough to be a table cell. Whitespace — including
// the newlines a multi-line prompt is full of — collapses to single spaces, because a cell cannot
// render a newline and an un-collapsed one would just be invisible width. The cut counts RUNES and the
// ellipsis is only appended when something was actually removed.
func displayLabel(raw string) string {
	fields := strings.FieldsFunc(raw, unicode.IsSpace)
	flat := strings.Join(fields, " ")
	runes := []rune(flat)
	if len(runes) <= derivedNameRunes {
		return flat
	}
	return strings.TrimRight(string(runes[:derivedNameRunes]), " ") + "…"
}
