package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// changesetCompiledEvent is journaled when a changeset is recorded, so a multi-client attach sees the
// run produced a first-class changeset (spec §30.6). It is in protocols/schemas/execution/event-types.json.
const changesetCompiledEvent = "changeset.compiled.v1"

// ToolCallRow is one completed tool-call ledger row — the raw record the changeset compiler projects
// from (spec §30.6, REP-005). Arguments/Result are the JSON the tool recorded; the compiler parses
// file-write path/hash out of them, so the changeset is derived from what the run DID, not its prose.
type ToolCallRow struct {
	ID        string
	Name      string
	Arguments string
	Result    string
}

// RunToolCalls reads a run's completed tool-call ledger in chronological order (spec §30.6). It is the
// authoritative, model-independent record the changeset is compiled from.
func (s *Store) RunToolCalls(ctx context.Context, tenant Tenant, runID string) ([]ToolCallRow, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("RunToolCalls"), runID, tenant.Organization, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("read tool-call ledger: %w", err)
	}
	defer rows.Close()
	var out []ToolCallRow
	for rows.Next() {
		var r ToolCallRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Arguments, &r.Result); err != nil {
			return nil, fmt.Errorf("scan tool-call row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunBaseCommit reads the run's latest preparation base commit (spec §30.3). found is false when the
// run prepared no repository — then there is no base to diff against and no changeset to compile.
func (s *Store) RunBaseCommit(ctx context.Context, tenant Tenant, runID string) (string, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var base string
	err := s.pool.QueryRow(ctx, storage.Query("RunBaseCommit"), runID, tenant.Organization, tenant.Project).Scan(&base)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read run base commit: %w", err)
	}
	return base, true, nil
}

// ChangesetFile is one changed file in a changeset, compiled from a file-tool write (spec §30.6):
// its path, the change kind (added/modified), the content hash before and after, and the tool call
// that made it — the authoring lineage that ties the summary back to the ledger.
type ChangesetFile struct {
	Path       string `json:"path"`
	Change     string `json:"change"`
	BeforeHash string `json:"before_hash,omitempty"`
	AfterHash  string `json:"after_hash,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ChangesetFinding is one likely-committed-secret finding over a file entering the changeset (spec
// §30.4/§30.6). Kind is "secret"; Rule names the matched shape; Path is the file it hit.
type ChangesetFinding struct {
	ID   string
	Kind string
	Path string
	Rule string
}

// ChangesetRecord is a first-class, immutable changeset ready to persist (spec §30.6). ContentHash is
// its content address — equal ledgers produce an equal record, so there is no update path.
type ChangesetRecord struct {
	ID                string
	RunID             string
	BaseCommit        string
	FinalCommit       string
	FinalTree         string
	Files             []ChangesetFile
	PatchArtifactID   string
	TestLogArtifactID string
	PatchTruncated    bool
	ContentHash       string
	Findings          []ChangesetFinding
}

// RecordChangeset persists an immutable changeset, its findings, and a changeset.compiled event in one
// transaction (spec §30.6). The id is content-addressed (execution.changesetID), so a re-compile of
// the same ledger inserts 0 rows on the primary key — genuinely idempotent: the findings and the
// compiled event are emitted only on the FIRST record, never duplicated by an E10 replay.
// sessionID/responseID scope the journal event.
func (s *Store) RecordChangeset(ctx context.Context, tenant Tenant, sessionID, responseID string, rec ChangesetRecord) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	files, err := json.Marshal(nonNilFiles(rec.Files))
	if err != nil {
		return fmt.Errorf("marshal changeset files: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin changeset commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, storage.Query("InsertChangeset"),
		rec.ID, tenant.Organization, tenant.Project, rec.RunID, rec.BaseCommit, rec.FinalCommit,
		rec.FinalTree, files, nullableText(rec.PatchArtifactID), nullableText(rec.TestLogArtifactID),
		rec.PatchTruncated, rec.ContentHash)
	if err != nil {
		return fmt.Errorf("insert changeset: %w", err)
	}
	// Findings + the compiled event ride the FIRST insert only. A re-compile conflicts on the id
	// (0 rows), so this block is skipped and neither is duplicated.
	if tag.RowsAffected() > 0 {
		for _, f := range rec.Findings {
			kind := f.Kind
			if kind == "" {
				kind = "secret"
			}
			if _, err := tx.Exec(ctx, storage.Query("InsertChangesetFinding"),
				f.ID, rec.ID, tenant.Organization, tenant.Project, kind, f.Path, f.Rule); err != nil {
				return fmt.Errorf("insert changeset finding: %w", err)
			}
		}
		payload, _ := json.Marshal(map[string]any{"run_id": rec.RunID, "changeset_id": rec.ID, "content_hash": rec.ContentHash})
		if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, changesetCompiledEvent, payload); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit changeset: %w", err)
	}
	return nil
}

// nonNilFiles returns a non-nil slice so an empty file set marshals as [] not null.
func nonNilFiles(f []ChangesetFile) []ChangesetFile {
	if f == nil {
		return []ChangesetFile{}
	}
	return f
}
