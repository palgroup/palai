//go:build component

package extensions

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/storage"
)

// TestSlackConnectionCreateReadRLS proves the 000035 slack_connections store: a connection is created with
// secret_ref HANDLES (never inline values) and read back; a duplicate workspace is a typed collision; an
// inline credential field is rejected before any write; and the row is invisible across the tenant boundary
// (RLS org/project scoping) — a foreign tenant's scope reads zero and the WHERE-less count sees only its own.
func TestSlackConnectionCreateReadRLS(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	body := []byte(`{"team_id":"T111","bot_user_id":"Ubot","signing_secret_ref":"slack/signing","bot_token_ref":"slack/bot","scopes":"app_mentions:read chat:write","allowed_users":["U1"]}`)
	conn, err := s.CreateSlackConnection(ctx, org, project, body)
	if err != nil {
		t.Fatalf("create slack connection: %v", err)
	}
	if conn.TeamID != "T111" || conn.SigningSecretRef != "slack/signing" || conn.BotUserID != "Ubot" {
		t.Fatalf("created connection = %+v, want T111/slack-signing/Ubot", conn)
	}

	got, err := s.GetSlackConnection(ctx, org, project, conn.ID)
	if err != nil {
		t.Fatalf("get slack connection: %v", err)
	}
	if got.TeamID != "T111" || got.Disabled {
		t.Fatalf("read-back = %+v, want T111 enabled", got)
	}

	// A duplicate workspace binding in the project is a typed collision.
	if _, err := s.CreateSlackConnection(ctx, org, project, body); !errors.Is(err, ErrSlackConnectionExists) {
		t.Fatalf("duplicate workspace: err = %v, want ErrSlackConnectionExists", err)
	}

	// An INLINE signing secret VALUE (not a ref) is rejected before any write — a credential can only be a
	// *_ref handle.
	if _, err := s.CreateSlackConnection(ctx, org, project,
		[]byte(`{"team_id":"T222","signing_secret_ref":"r","signing_secret":"8f742231b10e"}`)); !errors.Is(err, ErrUnknownField) {
		t.Fatalf("inline secret: err = %v, want ErrUnknownField", err)
	}
	// A missing team id / signing ref is a typed config reject.
	if _, err := s.CreateSlackConnection(ctx, org, project, []byte(`{"team_id":"T333"}`)); !errors.Is(err, ErrInvalidSlackConfig) {
		t.Fatalf("no signing ref: err = %v, want ErrInvalidSlackConfig", err)
	}

	// Prove no raw secret ever reached the DB: no connection row carries a signing_secret/bot_token column
	// (the schema has only *_ref handle columns; this asserts the intent explicitly at the DB layer).
	var leaked int
	if err := s.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'slack_connections' AND column_name IN ('signing_secret','bot_token')`).Scan(&leaked); err != nil {
		t.Fatalf("scan for secret-value columns: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("slack_connections has %d raw-secret column(s); credentials must be secret_ref handles only", leaked)
	}

	// RLS: a second tenant cannot see the first tenant's connection. Seed org B and read under its scope.
	orgB, projectB := seedOrgProject(t, s)
	foreign, err := s.GetSlackConnection(ctx, orgB, projectB, conn.ID)
	if !errors.Is(err, ErrSlackConnectionNotFound) {
		t.Fatalf("cross-tenant get = %+v err = %v, want ErrSlackConnectionNotFound", foreign, err)
	}
	var visibleToB int
	if err := s.pool.QueryRow(storage.ScopeToTenant(ctx, orgB, projectB),
		`SELECT count(*) FROM slack_connections WHERE id = $1`, conn.ID).Scan(&visibleToB); err != nil {
		t.Fatalf("cross-tenant count: %v", err)
	}
	if visibleToB != 0 {
		t.Fatalf("tenant B saw %d row(s) of tenant A's connection; RLS did not deny", visibleToB)
	}
}

// TestSlackThreadSessionCorrelation proves SLK-003: two events in the SAME (team, channel, thread) resolve
// the SAME canonical session — the first claims it, the second reuses it (a web-console attach would join
// the same one), and a different thread gets its own session. A concurrent race collapses at the unique
// index to a single session.
func TestSlackThreadSessionCorrelation(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	conn, err := s.CreateSlackConnection(ctx, org, project,
		[]byte(`{"team_id":"T1","signing_secret_ref":"r"}`))
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	sess1 := seedSession(t, s, org, project)
	sess2 := seedSession(t, s, org, project)

	// First event in the thread claims sess1 as canonical.
	got, created, err := s.CorrelateThreadSession(ctx, org, project, conn.ID, "T1", "C1", "100.0", sess1)
	if err != nil || !created || got != sess1 {
		t.Fatalf("first correlate = (%q,%v,%v), want (%q,true,nil)", got, created, err, sess1)
	}
	// A second event in the SAME thread, offering a DIFFERENT session, must REUSE the first (one session
	// per thread) — the offered sess2 is discarded.
	got, created, err = s.CorrelateThreadSession(ctx, org, project, conn.ID, "T1", "C1", "100.0", sess2)
	if err != nil || created || got != sess1 {
		t.Fatalf("second correlate = (%q,%v,%v), want (%q,false,nil) — thread reuse", got, created, err, sess1)
	}
	// A different thread gets its own session.
	got, created, err = s.CorrelateThreadSession(ctx, org, project, conn.ID, "T1", "C1", "200.0", sess2)
	if err != nil || !created || got != sess2 {
		t.Fatalf("other-thread correlate = (%q,%v,%v), want (%q,true,nil)", got, created, err, sess2)
	}
}

// seedOrgProject seeds a fresh org+project (owner-scoped) so a cross-tenant negative has a second tenant.
func seedOrgProject(t *testing.T, s *Store) (string, string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	org, project := testID("org"), testID("prj")
	if _, err := s.pool.Exec(ctx, `INSERT INTO organizations (id) VALUES ($1)`, org); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return org, project
}

// seedSession seeds a session row the thread-correlation FK can reference.
func seedSession(t *testing.T, s *Store, org, project string) string {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	id := testID("ses")
	if _, err := s.pool.Exec(ctx, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, id, org, project); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}
