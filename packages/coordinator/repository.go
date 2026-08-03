package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/storage"
)

// RepositoryBindingInput registers a durable binding for a project's external repository (spec
// §30.1). Display names and URLs are not trusted as identity — Provider + RepositoryIdentity (the
// provider installation/repository id) are authoritative. Policy is the submodule/LFS/commit-signing/
// branch/path/PR-target bundle; the raw credential is never stored, only ConnectionRef.
type RepositoryBindingInput struct {
	BindingID          string
	Provider           string
	RepositoryIdentity string
	CloneURL           string
	DefaultBranch      string
	ConnectionRef      string
	AllowedOperations  []string
	Policy             map[string]any
	DataClassification string
	RegionConstraint   string
}

// CreateRepositoryBinding stores a binding within the tenant scope (spec §30.1).
func (s *Store) CreateRepositoryBinding(ctx context.Context, tenant Tenant, in RepositoryBindingInput) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	allowed, err := json.Marshal(nonNilSlice(in.AllowedOperations))
	if err != nil {
		return fmt.Errorf("marshal allowed operations: %w", err)
	}
	policy := []byte("{}")
	if in.Policy != nil {
		if policy, err = json.Marshal(in.Policy); err != nil {
			return fmt.Errorf("marshal repository policy: %w", err)
		}
	}
	branch := in.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	_, err = s.pool.Exec(ctx, storage.Query("CreateRepositoryBinding"),
		in.BindingID, tenant.Organization, tenant.Project, in.Provider, in.RepositoryIdentity,
		in.CloneURL, branch, in.ConnectionRef, allowed, policy, in.DataClassification, in.RegionConstraint)
	if err != nil {
		return fmt.Errorf("create repository binding: %w", err)
	}
	return nil
}

// GetRepositoryBinding resolves a binding within tenant scope (spec §30.3 step 1). found is false
// for an unknown OR foreign id — existence is not disclosed across tenants.
func (s *Store) GetRepositoryBinding(ctx context.Context, tenant Tenant, id string) (contracts.RepositoryBinding, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var (
		binding    contracts.RepositoryBinding
		allowedRaw []byte
		policyRaw  []byte
		createdAt  time.Time
		archivedAt *time.Time
	)
	err := s.pool.QueryRow(ctx, storage.Query("GetRepositoryBinding"), id, tenant.Project).
		Scan(&binding.ID, &binding.Provider, &binding.RepositoryIdentity, &binding.CloneUrl, &binding.DefaultBranch,
			&binding.ConnectionRef, &allowedRaw, &policyRaw, &binding.DataClassification, &binding.RegionConstraint,
			&createdAt, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.RepositoryBinding{}, false, nil
	}
	if err != nil {
		return contracts.RepositoryBinding{}, false, fmt.Errorf("get repository binding: %w", err)
	}
	binding.Object = "repository_binding"
	binding.OrganizationID = contracts.OrganizationID(tenant.Organization)
	binding.ProjectID = contracts.ProjectID(tenant.Project)
	binding.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	// AN ARCHIVED BINDING IS STILL READABLE, and that is the point of the read route keeping it: the
	// admission guard answers "not found" for a retired binding (RepositoryBindingExists), so this is the
	// only surface that can tell an operator WHY their run was refused. `omitempty` on the generated field
	// means a live binding carries no key at all rather than a null.
	if archivedAt != nil {
		binding.ArchivedAt = archivedAt.UTC().Format(time.RFC3339)
	}
	_ = json.Unmarshal(allowedRaw, &binding.AllowedOperations)
	_ = json.Unmarshal(policyRaw, &binding.Policy)
	return binding, true, nil
}

// SetRepositoryBindingConnection points a binding at a different credential handle, or at none (E30,
// migration 000057). found is false for an unknown, foreign, OR archived id — the same answer for the
// same reason the reads give it, and the archived case is deliberate: nothing can run against a retired
// binding, so re-crediting one would be a write with no effect anybody could observe.
//
// ref MAY BE EMPTY, and that is the detach direction: the repository became public, or the token was
// retired and the binding should fall back to whatever the deployment's global broker offers. Refusing it
// would leave the same dead end this method exists to remove, one step further along.
//
// THE RAW CREDENTIAL DOES NOT PASS THROUGH HERE AND CANNOT. ref is a secret_refs NAME; the bytes behind it
// are written over POST /v1/secret-refs and read at clone time by the resolver — this method moves a
// pointer, which is why it can be an ordinary tenant-scoped UPDATE with nothing sensitive in its
// parameters, its error text, or any log line that records it.
func (s *Store) SetRepositoryBindingConnection(ctx context.Context, tenant Tenant, id, ref string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var updated string
	err := s.pool.QueryRow(ctx, storage.Query("SetRepositoryBindingConnection"),
		id, tenant.Project, ref).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("set repository binding connection: %w", err)
	}
	return true, nil
}

// ArchiveRepositoryBinding retires a binding: it leaves the default list and, through the
// `archived_at IS NULL` clause on RepositoryBindingExists, refuses new runs. changed is false when the
// id is unknown, foreign, or ALREADY archived — the query's own predicate makes it idempotent, so a
// second call cannot rewrite when the retirement happened.
//
// It is not a delete: preparation_receipts.repository_binding_id is a foreign key onto this table, and
// the provenance those rows carry outlives the binding by design.
func (s *Store) ArchiveRepositoryBinding(ctx context.Context, tenant Tenant, id string) (bool, error) {
	return s.flipArchive(ctx, tenant, id, "ArchiveRepositoryBinding")
}

// UnarchiveRepositoryBinding puts a retired binding back in service. changed is false for an unknown,
// foreign, or already-live id. Archiving is operator hygiene rather than a security decision, so it is
// reversible; taking a binding's ACCESS away is the other method on this file — detach its connection.
func (s *Store) UnarchiveRepositoryBinding(ctx context.Context, tenant Tenant, id string) (bool, error) {
	return s.flipArchive(ctx, tenant, id, "UnarchiveRepositoryBinding")
}

// flipArchive is the shared body of the two above. They differ only in which statement they issue — both
// are tenant-scoped, both are idempotent by predicate, and both report "did this call change anything"
// the same way — so one implementation is one place for the scope declaration to be right.
func (s *Store) flipArchive(ctx context.Context, tenant Tenant, id, query string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var updated string
	err := s.pool.QueryRow(ctx, storage.Query(query), id, tenant.Organization, tenant.Project).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", query, err)
	}
	return true, nil
}

// PreparationReceiptInput records the model-independent preparation provenance (spec §30.3 step 10,
// REP-001). RunID may be empty (a session-only or test preparation).
type PreparationReceiptInput struct {
	ReceiptID    string
	BindingID    string
	RunID        string
	RequestedRef string
	BaseCommit   string
	TreeHash     string
	Branch       string
}

// RecordPreparationReceipt persists a preparation receipt (append-only per attempt, spec §30.3).
func (s *Store) RecordPreparationReceipt(ctx context.Context, tenant Tenant, in PreparationReceiptInput) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	_, err := s.pool.Exec(ctx, storage.Query("RecordPreparationReceipt"),
		in.ReceiptID, in.BindingID, tenant.Organization, tenant.Project, nullableText(in.RunID),
		in.RequestedRef, in.BaseCommit, in.TreeHash, in.Branch)
	if err != nil {
		return fmt.Errorf("record preparation receipt: %w", err)
	}
	return nil
}

// GetPreparationReceipt reads the latest recorded receipt for a binding+run — its exact-commit
// provenance. found is false when no preparation has been recorded.
func (s *Store) GetPreparationReceipt(ctx context.Context, bindingID, runID string) (contracts.PreparationReceipt, bool, error) {
	var (
		receipt    contracts.PreparationReceipt
		preparedAt time.Time
	)
	err := s.pool.QueryRow(ctx, storage.Query("GetPreparationReceipt"), bindingID, nullableText(runID)).
		Scan(&receipt.RequestedRef, &receipt.BaseCommit, &receipt.TreeHash, &receipt.Branch, &preparedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.PreparationReceipt{}, false, nil
	}
	if err != nil {
		return contracts.PreparationReceipt{}, false, fmt.Errorf("get preparation receipt: %w", err)
	}
	receipt.PreparedAt = preparedAt.UTC().Format(time.RFC3339)
	return receipt, true, nil
}

// nonNilSlice returns a non-nil slice so an empty AllowedOperations marshals as [] not null.
func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
