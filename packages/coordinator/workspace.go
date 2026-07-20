package coordinator

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// ErrWriterLeaseHeld is returned when a second writer tries to lease a workspace that already
// has an active writer lease (spec §29.8 single-writer). Its stable Problem code is
// lease_conflict; the DB partial unique index (workspace_leases_one_active_writer) is the
// authority, so this only translates the 23505 into a typed conflict.
var ErrWriterLeaseHeld = errors.New("lease_conflict")

// ErrStaleAllocation is returned when a stale allocation tries to record an authoritative
// snapshot after fencing has advanced (spec §29.8 line 3070, SAN-006). Its Problem code is
// lease_conflict. The guarded insert affects zero rows for a non-current allocation, so the
// reject is at the DB, not an app-code check.
var ErrStaleAllocation = errors.New("lease_conflict")

// WorkspaceInput opens a logical workspace bound to one session/run (spec §29.7). RunID may be
// empty (a session-only binding). UnsafeBind + UnsafeHostPath + PublicationDisabled carry the
// §30.13 unsafe-local-bind decision the caller resolved (REP-012).
type WorkspaceInput struct {
	WorkspaceID         string
	SessionID           string
	RunID               string
	State               string
	UnsafeBind          bool
	UnsafeHostPath      string
	PublicationDisabled bool
}

// Allocation is one physical allocation of a logical workspace: a distinct allocation id, its
// fencing token, and the opaque host directory the supervisor mounts to /workspace (spec §29.7).
type Allocation struct {
	ID       string
	Fence    int64
	HostPath string
}

// SnapshotInput is a create-side workspace snapshot manifest (spec §29.10). The checksums and
// exclusions are computed over the real allocation filesystem; RESTORE is E10.
type SnapshotInput struct {
	SnapshotID    string
	AllocationID  string
	TreeChecksum  string
	IndexChecksum string
	FileChecksums []byte
	Exclusions    []byte
	Reason        string
}

// SessionWorkspace is a session's attached coding workspace (spec §29.7, E09 Task 10): the logical
// workspace id the root run auto-provisions, the repository binding + requested ref it clones, and its
// current lifecycle state (which the root run drives requested→provisioning→preparing→ready→leased).
type SessionWorkspace struct {
	WorkspaceID  string
	BindingID    string
	RequestedRef string
	State        string
}

// WorkspaceForSession resolves the session's attached coding workspace within tenant scope. found is
// false for a session with no attached binding — the root run then provisions nothing (pre-E09
// behaviour). It is a by-session read the root run makes at start to learn what to provision.
func (s *Store) WorkspaceForSession(ctx context.Context, tenant Tenant, sessionID string) (SessionWorkspace, bool, error) {
	var ws SessionWorkspace
	err := s.pool.QueryRow(ctx, storage.Query("WorkspaceForSession"), sessionID, tenant.Organization, tenant.Project).
		Scan(&ws.WorkspaceID, &ws.BindingID, &ws.RequestedRef, &ws.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionWorkspace{}, false, nil
	}
	if err != nil {
		return SessionWorkspace{}, false, fmt.Errorf("workspace for session %s: %w", sessionID, err)
	}
	return ws, true, nil
}

// AdvanceWorkspace applies one WorkspaceTable transition to a workspace's lifecycle state (spec §29.7),
// the workspace analogue of ApplyRunTransition: it locks the row, asks the pure WorkspaceTable whether
// the command is legal from the current state, and writes the new state — an illegal transition is
// rejected before any write. It journals no session event (the workspace lifecycle is a projection, not
// part of the session journal in E09) and takes no outbox row.
func (s *Store) AdvanceWorkspace(ctx context.Context, tenant Tenant, workspaceID string, command statemachines.WorkspaceCommand) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin workspace transition: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var current string
	err = tx.QueryRow(ctx, storage.Query("WorkspaceState"), workspaceID, tenant.Organization, tenant.Project).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("advance workspace: %s not found in tenant scope", workspaceID)
	}
	if err != nil {
		return fmt.Errorf("lock workspace: %w", err)
	}
	next, _, err := statemachines.Apply(statemachines.WorkspaceState(current), command, statemachines.WorkspaceTable)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, storage.Query("UpdateWorkspaceState"), workspaceID, tenant.Organization, tenant.Project, string(next)); err != nil {
		return fmt.Errorf("update workspace state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit workspace transition: %w", err)
	}
	return nil
}

// CreateWorkspace opens a logical workspace in the requested binding state (spec §29.7).
func (s *Store) CreateWorkspace(ctx context.Context, tenant Tenant, in WorkspaceInput) error {
	state := in.State
	if state == "" {
		state = "requested"
	}
	_, err := s.pool.Exec(ctx, storage.Query("CreateWorkspace"),
		in.WorkspaceID, tenant.Organization, tenant.Project, in.SessionID, nullableText(in.RunID),
		state, in.UnsafeBind, in.UnsafeHostPath, in.PublicationDisabled)
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	return nil
}

// AllocateWorkspace mints a new physical allocation with the next fencing token (max+1). The
// logical workspace id is unchanged — a host move only ever adds a strictly higher fence (spec
// §29.7). A racing duplicate fence is a unique_violation the caller may retry.
func (s *Store) AllocateWorkspace(ctx context.Context, allocationID, workspaceID, hostPath string) (Allocation, error) {
	alloc := Allocation{HostPath: hostPath}
	err := s.pool.QueryRow(ctx, storage.Query("AllocateWorkspace"), allocationID, workspaceID, hostPath).
		Scan(&alloc.ID, &alloc.Fence)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, fmt.Errorf("allocate workspace: workspace %s not found", workspaceID)
	}
	if err != nil {
		return Allocation{}, fmt.Errorf("allocate workspace: %w", err)
	}
	return alloc, nil
}

// CurrentAllocation returns the workspace's current (max-fence) allocation — the one the
// supervisor mounts and the only one that may upload an authoritative snapshot (spec §29.8).
func (s *Store) CurrentAllocation(ctx context.Context, workspaceID string) (Allocation, error) {
	var alloc Allocation
	err := s.pool.QueryRow(ctx, storage.Query("CurrentAllocation"), workspaceID).
		Scan(&alloc.ID, &alloc.Fence, &alloc.HostPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, fmt.Errorf("current allocation: workspace %s has none", workspaceID)
	}
	if err != nil {
		return Allocation{}, fmt.Errorf("current allocation: %w", err)
	}
	return alloc, nil
}

// AcquireWriterLease takes the single active writer lease for the allocation's workspace, held by
// runID (spec §29.8). A second concurrent active lease is rejected by the partial unique index;
// this maps that 23505 to ErrWriterLeaseHeld. A lease on a SUPERSEDED allocation — one a host move
// has fenced out — affects zero rows via the query's fence-currency guard and is rejected as a stale
// allocation, so a fenced-out writer cannot re-acquire authority (spec §29.8, SAN-006).
func (s *Store) AcquireWriterLease(ctx context.Context, leaseID, allocationID, runID string) error {
	tag, err := s.pool.Exec(ctx, storage.Query("AcquireWriterLease"), leaseID, allocationID, runID)
	if isUniqueViolation(err) {
		return ErrWriterLeaseHeld
	}
	if err != nil {
		return fmt.Errorf("acquire writer lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleAllocation
	}
	return nil
}

// ReleaseWriterLease frees the single-writer slot for the next writer (spec §29.8).
func (s *Store) ReleaseWriterLease(ctx context.Context, leaseID string) error {
	if _, err := s.pool.Exec(ctx, storage.Query("ReleaseWriterLease"), leaseID); err != nil {
		return fmt.Errorf("release writer lease: %w", err)
	}
	return nil
}

// CreateWorkspaceSnapshot records a create-side snapshot, but only when the uploading allocation
// is the workspace's current (max-fence) one. A stale allocation affects zero rows — the DB-level
// reject of a stale authoritative snapshot after fencing advances (spec §29.8 line 3070, SAN-006).
func (s *Store) CreateWorkspaceSnapshot(ctx context.Context, in SnapshotInput) error {
	fileChecksums := in.FileChecksums
	if len(fileChecksums) == 0 {
		fileChecksums = []byte("{}")
	}
	exclusions := in.Exclusions
	if len(exclusions) == 0 {
		exclusions = []byte("[]")
	}
	tag, err := s.pool.Exec(ctx, storage.Query("CreateWorkspaceSnapshot"),
		in.SnapshotID, in.AllocationID, in.TreeChecksum, in.IndexChecksum, fileChecksums, exclusions, in.Reason)
	if err != nil {
		return fmt.Errorf("create workspace snapshot: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleAllocation
	}
	return nil
}
