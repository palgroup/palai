package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// exclusions are computed over the real allocation filesystem. ObjectKey/ArchiveChecksum/SizeBytes
// locate + verify the byte-archive (E10 Task 6, SAN-005 restore); they are empty/0 for a manifest-only
// (E09) snapshot with no archived bytes.
type SnapshotInput struct {
	SnapshotID      string
	AllocationID    string
	TreeChecksum    string
	IndexChecksum   string
	FileChecksums   []byte
	Exclusions      []byte
	Reason          string
	ObjectKey       string
	ArchiveChecksum string
	SizeBytes       int64
}

// WorkspaceSnapshotRecord is a persisted snapshot's byte-archive location plus its create-side manifest
// checksums (spec §29.10) — the facts a restore needs to fetch the archived bytes and verify the
// restored tree re-derives EQUAL (SAN-005). ObjectKey is empty for a manifest-only (E09) snapshot.
type WorkspaceSnapshotRecord struct {
	WorkspaceID     string
	ObjectKey       string
	ArchiveChecksum string
	SizeBytes       int64
	TreeChecksum    string
	IndexChecksum   string
	FileChecksums   []byte
	Exclusions      []byte
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
		in.SnapshotID, in.AllocationID, in.TreeChecksum, in.IndexChecksum, fileChecksums, exclusions, in.Reason,
		in.ObjectKey, in.ArchiveChecksum, in.SizeBytes)
	if err != nil {
		return fmt.Errorf("create workspace snapshot: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleAllocation
	}
	return nil
}

// QuarantinedHost is one poisoned host: its identity, why it was quarantined, and when (spec §29
// SAN-008). In the local tier the id is the provision-root / runner identity a destroy failed under.
type QuarantinedHost struct {
	HostID        string
	Reason        string
	QuarantinedAt time.Time
}

// QuarantineHost marks a host poisoned by an allocation-destroy failure so no new allocation is placed
// on it (spec §29 SAN-008). Idempotent on host_id — a repeat failure re-quarantines without error.
func (s *Store) QuarantineHost(ctx context.Context, hostID, reason string) error {
	if _, err := s.pool.Exec(ctx, storage.Query("QuarantineHost"), hostID, reason); err != nil {
		return fmt.Errorf("quarantine host %s: %w", hostID, err)
	}
	return nil
}

// IsHostQuarantined reports whether a host is quarantined — the placement guard before a new allocation.
func (s *Store) IsHostQuarantined(ctx context.Context, hostID string) (bool, error) {
	var quarantined bool
	if err := s.pool.QueryRow(ctx, storage.Query("IsHostQuarantined"), hostID).Scan(&quarantined); err != nil {
		return false, fmt.Errorf("check host quarantine %s: %w", hostID, err)
	}
	return quarantined, nil
}

// ListQuarantinedHosts returns every quarantined host newest-first — the doctor's quarantine view.
func (s *Store) ListQuarantinedHosts(ctx context.Context) ([]QuarantinedHost, error) {
	rows, err := s.pool.Query(ctx, storage.Query("ListQuarantinedHosts"))
	if err != nil {
		return nil, fmt.Errorf("list quarantined hosts: %w", err)
	}
	defer rows.Close()
	var out []QuarantinedHost
	for rows.Next() {
		var h QuarantinedHost
		if err := rows.Scan(&h.HostID, &h.Reason, &h.QuarantinedAt); err != nil {
			return nil, fmt.Errorf("scan quarantined host: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// LatestRestorableWorkspaceSnapshot returns the id of the workspace's newest snapshot that carries
// archived bytes (spec §29.10, REC-005) — the boundary a host-lost recovery restores from. found is
// false when the workspace has no byte-archived snapshot: the recovery then has no boundary to restore
// and must fail explicitly rather than resume on an empty tree.
func (s *Store) LatestRestorableWorkspaceSnapshot(ctx context.Context, tenant Tenant, workspaceID string) (string, bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, storage.Query("LatestRestorableWorkspaceSnapshot"), workspaceID, tenant.Organization, tenant.Project).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("latest restorable snapshot: %w", err)
	}
	return id, true, nil
}

// LoadWorkspaceSnapshot reads a snapshot's byte-archive location + create-side manifest within tenant
// scope, so a restore fetches the archived bytes and verifies the restored tree (spec §29.10, SAN-005).
func (s *Store) LoadWorkspaceSnapshot(ctx context.Context, tenant Tenant, snapshotID string) (WorkspaceSnapshotRecord, error) {
	var rec WorkspaceSnapshotRecord
	err := s.pool.QueryRow(ctx, storage.Query("LoadWorkspaceSnapshot"), snapshotID, tenant.Organization, tenant.Project).
		Scan(&rec.WorkspaceID, &rec.ObjectKey, &rec.ArchiveChecksum, &rec.SizeBytes,
			&rec.TreeChecksum, &rec.IndexChecksum, &rec.FileChecksums, &rec.Exclusions)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceSnapshotRecord{}, fmt.Errorf("load workspace snapshot: %s not found in tenant scope", snapshotID)
	}
	if err != nil {
		return WorkspaceSnapshotRecord{}, fmt.Errorf("load workspace snapshot: %w", err)
	}
	return rec, nil
}
