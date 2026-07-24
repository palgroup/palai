package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// Store is the Postgres-backed a2a_interfaces + a2a_task_refs persistence (migration 000038). It implements
// the Server's InterfaceStore + Tasks seams. Every tenant read/write runs under a scoped context so RLS
// (000029/000038) confines the rows to the caller's tenant; the ONLY system-scoped read is the public card
// resolve, keyed by the server-minted interface id, returning only safe card columns (A2A-001).
type Store struct {
	pool  *pgxpool.Pool
	newID func(prefix string) string
}

// NewStore builds the store. newID mints row ids; pass middleware.NewID in production.
func NewStore(pool *pgxpool.Pool, newID func(prefix string) string) *Store {
	return &Store{pool: pool, newID: newID}
}

// Compile-time proof the store satisfies both seams the Server needs.
var (
	_ InterfaceStore = (*Store)(nil)
	_ Tasks          = (*Store)(nil)
)

func (s *Store) mintID(prefix string) string {
	if s.newID != nil {
		return s.newID(prefix)
	}
	return prefix + "_" + strconv.FormatInt(atomic.AddInt64(&storeSeq, 1), 36)
}

var storeSeq int64

// PublishInterface persists a projected interface (the a2a_interfaces row). The caller has already run
// ProjectInterface, so iface carries only SAFE card fields plus the tenant + revision pin. It returns the
// minted interface id.
func (s *Store) PublishInterface(ctx context.Context, iface PublishedInterface) (string, error) {
	id := iface.ID
	if id == "" {
		id = s.mintID("a2aif")
	}
	skills, err := json.Marshal(iface.Skills)
	if err != nil {
		return "", fmt.Errorf("marshal a2a skills: %w", err)
	}
	ctx = storage.WithTenant(ctx, iface.Organization, iface.Project)
	_, err = s.pool.Exec(ctx, storage.Query("InsertA2AInterface"),
		id, iface.Organization, iface.Project, iface.Name, iface.Description, iface.Version,
		iface.AgentProfileID, iface.AgentRevisionID, iface.Streaming, iface.PushNotifications, iface.ExtendedCard,
		iface.InputModes, iface.OutputModes, skills, iface.AuthScheme, true, iface.ETag)
	if err != nil {
		return "", fmt.Errorf("insert a2a interface: %w", err)
	}
	return id, nil
}

// ResolvePublic serves the UNAUTHENTICATED public card, keyed by interface id (system-scoped: there is no
// bearer scope on the public card route). It reads ONLY the card-visible safe columns — the query never
// SELECTs org/project or the agent_profile/agent_revision provenance pins (M-5), so the public path reaches
// nothing beyond what the card renders.
func (s *Store) ResolvePublic(ctx context.Context, interfaceID string) (PublishedInterface, bool, error) {
	ctx = storage.WithSystemScope(ctx)
	row := s.pool.QueryRow(ctx, storage.Query("ResolveA2AInterfacePublic"), interfaceID)
	var iface PublishedInterface
	var skills []byte
	err := row.Scan(&iface.ID, &iface.Name, &iface.Description, &iface.Version,
		&iface.Streaming, &iface.PushNotifications, &iface.ExtendedCard,
		&iface.InputModes, &iface.OutputModes, &skills, &iface.AuthScheme, &iface.ETag)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublishedInterface{}, false, nil
	}
	if err != nil {
		return PublishedInterface{}, false, fmt.Errorf("scan a2a public interface: %w", err)
	}
	if len(skills) > 0 {
		_ = json.Unmarshal(skills, &iface.Skills)
	}
	return iface, true, nil
}

// Get resolves an interface within the authenticated scope (the extended card + all authed ops). It reads
// the full row including the provenance pins (never rendered onto a card, but read for tenant scoping).
func (s *Store) Get(ctx context.Context, org, project, interfaceID string) (PublishedInterface, bool, error) {
	ctx = storage.WithTenant(ctx, org, project)
	row := s.pool.QueryRow(ctx, storage.Query("GetA2AInterface"), interfaceID, org, project)
	var iface PublishedInterface
	var skills []byte
	err := row.Scan(&iface.ID, &iface.Organization, &iface.Project, &iface.Name, &iface.Description, &iface.Version,
		&iface.AgentProfileID, &iface.AgentRevisionID, &iface.Streaming, &iface.PushNotifications, &iface.ExtendedCard,
		&iface.InputModes, &iface.OutputModes, &skills, &iface.AuthScheme, &iface.ETag)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublishedInterface{}, false, nil
	}
	if err != nil {
		return PublishedInterface{}, false, fmt.Errorf("scan a2a interface: %w", err)
	}
	if len(skills) > 0 {
		_ = json.Unmarshal(skills, &iface.Skills)
	}
	return iface, true, nil
}

// Put records the external->canonical task bridge (§38.2). run_id/session_id are the platform-minted
// canonical ids; they are stored, never replaced by anything the client supplies.
func (s *Store) Put(ctx context.Context, org, project string, ref TaskRef) error {
	ctx = storage.WithTenant(ctx, org, project)
	_, err := s.pool.Exec(ctx, storage.Query("InsertA2ATaskRef"),
		s.mintID("a2aref"), org, project, ref.InterfaceID, ref.A2ATaskID, ref.A2AContextID, ref.RunID, ref.SessionID)
	if err != nil {
		return fmt.Errorf("insert a2a task ref: %w", err)
	}
	return nil
}

// GetRef resolves a task ref within scope by its external a2a_task_id under an interface.
func (s *Store) GetRef(ctx context.Context, org, project, interfaceID, a2aTaskID string) (TaskRef, bool, error) {
	ctx = storage.WithTenant(ctx, org, project)
	var ref TaskRef
	var push []byte
	ref.InterfaceID = interfaceID
	err := s.pool.QueryRow(ctx, storage.Query("GetA2ATaskRef"), interfaceID, a2aTaskID, org, project).
		Scan(new(string), &ref.A2ATaskID, &ref.A2AContextID, &ref.RunID, &ref.SessionID, &push)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRef{}, false, nil
	}
	if err != nil {
		return TaskRef{}, false, fmt.Errorf("get a2a task ref: %w", err)
	}
	if len(push) > 0 {
		_ = json.Unmarshal(push, &ref.PushConfigs)
	}
	return ref, true, nil
}

// GetRefByRun resolves an existing task ref within scope by its canonical run reference under an interface —
// the A2A-retry dedupe seam (M-2). A replayed messageId re-admits to the same canonical response, so the
// external task minted the first time is reused rather than duplicated.
func (s *Store) GetRefByRun(ctx context.Context, org, project, interfaceID, runID string) (TaskRef, bool, error) {
	ctx = storage.WithTenant(ctx, org, project)
	ref := TaskRef{InterfaceID: interfaceID}
	var push []byte
	err := s.pool.QueryRow(ctx, storage.Query("GetA2ATaskRefByRun"), interfaceID, runID, org, project).
		Scan(new(string), &ref.A2ATaskID, &ref.A2AContextID, &ref.RunID, &ref.SessionID, &push)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRef{}, false, nil
	}
	if err != nil {
		return TaskRef{}, false, fmt.Errorf("get a2a task ref by run: %w", err)
	}
	if len(push) > 0 {
		_ = json.Unmarshal(push, &ref.PushConfigs)
	}
	return ref, true, nil
}

// List pages an interface's task refs newest-first (the tasks list endpoint).
func (s *Store) List(ctx context.Context, org, project, interfaceID string, limit int) ([]TaskRef, error) {
	if limit <= 0 {
		limit = 100
	}
	ctx = storage.WithTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListA2ATaskRefs"), interfaceID, org, project, limit)
	if err != nil {
		return nil, fmt.Errorf("list a2a task refs: %w", err)
	}
	defer rows.Close()
	var out []TaskRef
	for rows.Next() {
		ref := TaskRef{InterfaceID: interfaceID}
		var push []byte
		if err := rows.Scan(new(string), &ref.A2ATaskID, &ref.A2AContextID, &ref.RunID, &ref.SessionID, &push); err != nil {
			return nil, err
		}
		if len(push) > 0 {
			_ = json.Unmarshal(push, &ref.PushConfigs)
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// SetPushConfigs replaces a task's push-config array (set/delete both write the whole array).
func (s *Store) SetPushConfigs(ctx context.Context, org, project, interfaceID, a2aTaskID string, cfgs []PushNotificationConfig) error {
	blob, err := json.Marshal(cfgs)
	if err != nil {
		return fmt.Errorf("marshal a2a push configs: %w", err)
	}
	ctx = storage.WithTenant(ctx, org, project)
	_, err = s.pool.Exec(ctx, storage.Query("UpdateA2ATaskPushConfigs"), interfaceID, a2aTaskID, org, project, blob)
	if err != nil {
		return fmt.Errorf("update a2a push configs: %w", err)
	}
	return nil
}
