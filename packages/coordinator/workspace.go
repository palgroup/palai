package coordinator

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

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
// this maps that 23505 to ErrWriterLeaseHeld.
func (s *Store) AcquireWriterLease(ctx context.Context, leaseID, allocationID, runID string) error {
	tag, err := s.pool.Exec(ctx, storage.Query("AcquireWriterLease"), leaseID, allocationID, runID)
	if isUniqueViolation(err) {
		return ErrWriterLeaseHeld
	}
	if err != nil {
		return fmt.Errorf("acquire writer lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("acquire writer lease: allocation %s not found", allocationID)
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
