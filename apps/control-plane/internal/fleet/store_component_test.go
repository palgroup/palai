//go:build component

package fleet_test

// The registry against a REAL Postgres with RLS actually enforced. It lives here rather than under
// tests/ because internal/fleet is an internal package Go forbids importing from outside
// apps/control-plane, and it is a component test rather than a unit one because every property under
// proof is a property of the DATABASE: that the row and its journal entry commit together, that an
// unknown pool refuses instead of writing a dangling reference, and that one tenant's read cannot see
// another's machines.
//
// It is wired into scripts/test/component's postgres suite by NAME. A component test this repository's
// selector does not name is a component test that never runs and reports the same green as one that
// passes — the trap this tree has fallen into seven times.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/storage"
)

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// enrollment is one machine's registration as the SERVER builds it: a minted id and the certificate
// name derived from that id. The pairing is written once, here, because writing it out at each call
// site is how the two drifted apart in the first place.
func enrollment(poolID, label string) fleet.Registration {
	id := newID("rnr")
	return fleet.Registration{ID: id, PoolID: poolID, Label: label, DNS: id + ".runners.palai.internal"}
}

// tenantFixture seeds an organization, a project and a runner pool, and returns the three ids.
func tenantFixture(t *testing.T, pool *pgxpool.Pool, posture string) (project, poolID string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	project, poolID = newID("prj"), newID("pool")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO projects (id) VALUES ($1)`, project)
	exec(`INSERT INTO runner_pools (id, project_id, name, posture) VALUES ($1,$2,$3,$4)`,
		poolID, project, newID("pool-name"), posture)
	return project, poolID
}

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	pool, err := storage.OpenPool(context.Background(), url)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestRegistryRecordsEachMachineOnceWithItsOwnJournalEntry is the durable half of both RED proofs. Two
// machines enrol into ONE pool sending the SAME label — the compose default, because
// runner-entrypoint.sh:10 hardcodes PALAI_RUNNER_ID="runner-local" — and the registry has to hold two
// rows, two server-minted ids, and two journal entries.
func TestRegistryRecordsEachMachineOnceWithItsOwnJournalEntry(t *testing.T) {
	pool := openPool(t)
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()

	one, two := enrollment(poolID, "runner-local"), enrollment(poolID, "runner-local")
	one.PublicKeySHA256, one.OS, one.Arch = "aa", "darwin", "arm64"
	two.PublicKeySHA256, two.OS, two.Arch = "bb", "darwin", "arm64"

	first, err := registry.Register(ctx, one)
	if err != nil {
		t.Fatalf("register first machine: %v", err)
	}
	second, err := registry.Register(ctx, two)
	if err != nil {
		t.Fatalf("register second machine sharing the label: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("two machines were recorded under one id %q", first.ID)
	}
	// Each row is addressable by the certificate name it was enrolled under. The store refuses the pair
	// apart (see TestRegisterRefusesAnIdentityItsDNSDoesNotName), so this is the durable half of that:
	// what came back is what a later lookup by SAN will resolve.
	for _, pair := range [][2]fleet.Runner{{first, fleet.Runner{ID: one.ID, DNS: one.DNS}}, {second, fleet.Runner{ID: two.ID, DNS: two.DNS}}} {
		if pair[0].ID != pair[1].ID || pair[0].DNS != pair[1].DNS {
			t.Fatalf("the stored row (%s, %s) is not the identity that was registered (%s, %s)",
				pair[0].ID, pair[0].DNS, pair[1].ID, pair[1].DNS)
		}
	}
	// The tenant came off the POOL, not off anything the machine sent — the machine sends no tenant.
	if first.Project != project {
		t.Fatalf("runner project = %q, want the pool's %q", first.Project, project)
	}
	// A pool IS a posture, so its members inherit it. A runner that could declare its own would be a
	// runner that could ask to be placed somewhere it is not.
	if first.Posture != "unsandboxed-host" {
		t.Fatalf("runner posture = %q, want the pool's unsandboxed-host", first.Posture)
	}

	// The journal entry commits WITH the row. A certificate the journal does not record is a
	// certificate no targeted revoke can find (§3.6 D5).
	ctxSys := storage.WithSystemScope(ctx)
	for _, r := range []fleet.Runner{first, second} {
		var kind string
		var seq int64
		if err := pool.QueryRow(ctxSys,
			`SELECT entry_kind, entry_seq FROM runner_enrollments WHERE runner_id = $1`, r.ID).Scan(&kind, &seq); err != nil {
			t.Fatalf("read the journal entry for %s: %v", r.ID, err)
		}
		if kind != "issued" || seq != 1 {
			t.Fatalf("journal entry for %s = (%q, %d), want (issued, 1)", r.ID, kind, seq)
		}
	}
	// NO CREDENTIAL IN THE JOURNAL (§2). Asserted as an EXACT key set rather than by scanning for
	// credential-shaped words, because that scan is the vacuous version twice over: `public_key_sha256`
	// contains "key" and would fire on an entirely correct row, and a field named `t` holding a token
	// would not fire at all. An exact set means a field added later has to be added here too, in front
	// of a reviewer, which is the only check that keeps working as the shape grows.
	var detail map[string]any
	if err := pool.QueryRow(ctxSys,
		`SELECT detail FROM runner_enrollments WHERE runner_id = $1`, first.ID).Scan(&detail); err != nil {
		t.Fatalf("read the journal detail: %v", err)
	}
	want := map[string]bool{"label": true, "runner_dns": true, "public_key_sha256": true}
	for key := range detail {
		if !want[key] {
			t.Fatalf("the journal detail carries an unexpected field %q; every field here is public by "+
				"construction and a new one has to be justified: %v", key, detail)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("the journal detail is missing %v", want)
	}
}

// TestRegisterRefusesAnUnknownPool: enrolling into a pool that is not there must REFUSE, not default.
// The pool is what resolves the tenant for a wire that carries none, so a default would be a tenant
// this code invented.
func TestRegisterRefusesAnUnknownPool(t *testing.T) {
	pool := openPool(t)
	registry := fleet.NewStore(pool, newID, nil)
	if _, err := registry.Register(context.Background(), enrollment("pool_does_not_exist", "runner-local")); !errors.Is(err, fleet.ErrUnknownPool) {
		t.Fatalf("register into an unknown pool = %v, want ErrUnknownPool", err)
	}
	var orphans int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runners WHERE pool_id = 'pool_does_not_exist'`).Scan(&orphans); err != nil {
		t.Fatalf("count orphaned runners: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d runner row(s) were written for a pool that does not exist", orphans)
	}
}

// TestRegisterRefusesAnIdentityItsDNSDoesNotName is the guard on the bug this task shipped and then
// caught: a row is resolved by its certificate SAN for the whole of its life, so a row whose recorded
// SAN is not derived from its own id is a row NOTHING can find again — last_seen_at could never
// advance for any runner. It happened because two places minted an id. The store now refuses the pair
// apart, and the refusal is asserted here in all three shapes it can arrive in.
func TestRegisterRefusesAnIdentityItsDNSDoesNotName(t *testing.T) {
	pool := openPool(t)
	_, poolID := tenantFixture(t, pool, "sandboxed-linux")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()

	mismatched := enrollment(poolID, "runner-local")
	mismatched.DNS = newID("rnr") + ".runners.palai.internal" // a name minted somewhere else
	noID := enrollment(poolID, "runner-local")
	noID.ID = ""
	noDNS := enrollment(poolID, "runner-local")
	noDNS.DNS = ""

	for name, reg := range map[string]fleet.Registration{
		"a DNS derived from another id": mismatched,
		"no id at all":                  noID,
		"no DNS at all":                 noDNS,
	} {
		if _, err := registry.Register(ctx, reg); !errors.Is(err, fleet.ErrIdentityMismatch) {
			t.Fatalf("register with %s = %v, want ErrIdentityMismatch", name, err)
		}
	}
	// And nothing was written: a refusal that leaves a row behind is not a refusal.
	var orphans int
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM runners WHERE pool_id = $1`, poolID).Scan(&orphans); err != nil {
		t.Fatalf("count runners in the fixture pool: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d runner row(s) survived a refused registration", orphans)
	}
}

// TestRecordSeenAdvancesLivenessAndReportsAnUnknownCertificate covers both halves of the ceiling: a
// known certificate advances the stamp, and an UNKNOWN one is found=false with no error — because that
// is a runner enrolled before this registry existed, reconnecting after an upgrade, and refusing it
// would break §2's bit-unchanged rule.
func TestRecordSeenAdvancesLivenessAndReportsAnUnknownCertificate(t *testing.T) {
	pool := openPool(t)
	_, poolID := tenantFixture(t, pool, "sandboxed-linux")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()

	reg := enrollment(poolID, "runner-local")
	dns := reg.DNS
	row, err := registry.Register(ctx, reg)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !row.CertNotAfter.IsZero() {
		t.Fatalf("a freshly registered runner already carries a certificate expiry %s; enrolment records the machine, the certificate is signed after", row.CertNotAfter)
	}

	seenAt := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	notAfter := seenAt.Add(5 * time.Minute)
	updated, found, err := registry.RecordSeen(ctx, dns, notAfter, seenAt)
	if err != nil || !found {
		t.Fatalf("RecordSeen on a known certificate = (found=%v, %v), want found", found, err)
	}
	if !updated.LastSeenAt.Equal(seenAt) || !updated.CertNotAfter.Equal(notAfter) {
		t.Fatalf("RecordSeen wrote (last_seen=%s, not_after=%s), want (%s, %s)",
			updated.LastSeenAt, updated.CertNotAfter, seenAt, notAfter)
	}
	// A connect presents a certificate it already holds and passes no fresher expiry; the recorded one
	// must not be erased back to null by a zero time.
	again, _, err := registry.RecordSeen(ctx, dns, time.Time{}, seenAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("second RecordSeen: %v", err)
	}
	if !again.CertNotAfter.Equal(notAfter) {
		t.Fatalf("a RecordSeen carrying no expiry erased the recorded one: %s", again.CertNotAfter)
	}

	if _, found, err := registry.RecordSeen(ctx, "runner-local.runners.palai.internal", time.Time{}, seenAt); err != nil || found {
		t.Fatalf("RecordSeen on a pre-registry runner's certificate = (found=%v, %v), want (false, nil) — an upgraded stack must not refuse its own runner", found, err)
	}
}

// TestReadsAreConfinedToTheCallersTenant drives Get and List under RLS. Two tenants each enrol a
// machine; neither can see nor address the other's, and the miss is a plain not-found so the caller
// never learns whether the id exists elsewhere.
func TestReadsAreConfinedToTheCallersTenant(t *testing.T) {
	pool := openPool(t)
	projectA, poolA := tenantFixture(t, pool, "sandboxed-linux")
	projectB, poolB := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()

	mine, err := registry.Register(ctx, enrollment(poolA, "a"))
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	theirs, err := registry.Register(ctx, enrollment(poolB, "b"))
	if err != nil {
		t.Fatalf("register B: %v", err)
	}

	if _, found, err := registry.Get(ctx, projectA, theirs.ID); err != nil || found {
		t.Fatalf("tenant A read tenant B's runner (found=%v, %v)", found, err)
	}
	if _, found, err := registry.Get(ctx, projectA, mine.ID); err != nil || !found {
		t.Fatalf("tenant A could not read its OWN runner (found=%v, %v)", found, err)
	}

	// THE LIST IS ASSERTED TENANT-SCOPED, NOT GLOBALLY. This package shares one Postgres with every
	// other component leg, so "the list has exactly one row" would be an assertion about whatever else
	// ran, not about this test — the shape that defeated an E19 T6 outbound assertion and an E21 T7
	// migration test on a shared fixture.
	page, err := registry.List(ctx, projectA, fleet.ListWindow{Limit: 50})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(page) != 1 || page[0].ID != mine.ID {
		t.Fatalf("tenant A's page = %d row(s) %v, want exactly its own runner %s", len(page), ids(page), mine.ID)
	}
	page, err = registry.List(ctx, projectB, fleet.ListWindow{Limit: 50})
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(page) != 1 || page[0].ID != theirs.ID {
		t.Fatalf("tenant B's page = %d row(s) %v, want exactly its own runner %s", len(page), ids(page), theirs.ID)
	}
	// The keyset coordinate the API's cursor is minted from has to actually be populated, or every
	// page after the first would be minted from the zero time.
	if page[0].CreatedAt.IsZero() {
		t.Fatal("a listed runner carries no created_at; the pagination cursor would be minted from the zero time")
	}
}

func ids(rows []fleet.Runner) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// TestEnrollingWithADeclaredPostureThePoolDoesNotHaveIsRefused is E24 T2's enrolment rule, and its
// two halves are DIFFERENT ASSERTIONS that this comment keeps apart because the code cannot:
//
//   - What is built: a machine STATES its posture at enrolment, the store COMPARES it with the pool's,
//     and a disagreement refuses the enrolment and records `refused` in the journal.
//   - What is NOT built: any verification that the statement is true. We cannot verify it — there is
//     no attestation on this wire — so a lying machine enrols into a sandboxed-linux pool and runs on
//     the host. That is `FLT-P2` in known-gaps-1.0.md, not a gap this test papers over.
//
// Catching a mismatch is worth having on its own: the realistic failure is an operator handing a Mac
// the Linux pool's enrolment key, which this refuses at the door instead of discovering when a run
// produces the wrong artefact.
func TestEnrollingWithADeclaredPostureThePoolDoesNotHaveIsRefused(t *testing.T) {
	pool := openPool(t)
	_, poolID := tenantFixture(t, pool, "sandboxed-linux")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()

	mac := enrollment(poolID, "runner-local")
	mac.Posture = "unsandboxed-host"
	if _, err := registry.Register(ctx, mac); !errors.Is(err, fleet.ErrPostureMismatch) {
		t.Fatalf("a machine declaring unsandboxed-host enrolled into a sandboxed-linux pool: err = %v, want ErrPostureMismatch", err)
	}

	ctxSys := storage.WithSystemScope(ctx)
	var rows int
	if err := pool.QueryRow(ctxSys, `SELECT count(*) FROM runners WHERE id = $1`, mac.ID).Scan(&rows); err != nil {
		t.Fatalf("count the refused machine's rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("the refused machine has %d registry row(s); a refusal that leaves a row is an admission", rows)
	}
	// THE REFUSAL IS RECORDED. A door that turns a machine away and writes nothing leaves the operator
	// with an enrolment that "just fails" and no way to see why — which is the state §3.6 D5 describes
	// for every other enrolment decision this tree makes.
	var kind, detail string
	if err := pool.QueryRow(ctxSys,
		`SELECT entry_kind, detail::text FROM runner_enrollments WHERE runner_id = $1`, mac.ID).Scan(&kind, &detail); err != nil {
		t.Fatalf("read the refusal journal entry: %v", err)
	}
	if kind != "refused" {
		t.Fatalf("journal entry kind = %q, want refused", kind)
	}
	if !strings.Contains(detail, "unsandboxed-host") || !strings.Contains(detail, "sandboxed-linux") {
		t.Fatalf("the refusal detail %q names neither what was declared nor what the pool is", detail)
	}

	// The same machine declaring what the pool IS enrols, and so does one declaring nothing — every
	// runner built before E24 sends no posture, and §2 says that machine's deployment is bit-unchanged.
	matching := enrollment(poolID, "runner-local")
	matching.Posture = "sandboxed-linux"
	if _, err := registry.Register(ctx, matching); err != nil {
		t.Fatalf("a machine declaring the pool's own posture was refused: %v", err)
	}
	silent := enrollment(poolID, "runner-local")
	row, err := registry.Register(ctx, silent)
	if err != nil {
		t.Fatalf("a machine declaring NO posture was refused; every pre-E24 runner declares none: %v", err)
	}
	if row.Posture != "sandboxed-linux" {
		t.Fatalf("a machine that declared nothing recorded posture %q, want the pool's", row.Posture)
	}
}

// TestListPoolsIsTenantScoped covers the read surface behind GET /v1/runner-pools. The assertion is
// scoped rather than global for the reason the runner list's is: this package shares one Postgres with
// every other component leg, so a count is an assertion about whatever else ran.
func TestListPoolsIsTenantScoped(t *testing.T) {
	db := openPool(t)
	projectA, poolA := tenantFixture(t, db, "unsandboxed-host")
	projectB, poolB := tenantFixture(t, db, "sandboxed-linux")
	registry := fleet.NewStore(db, newID, nil)
	ctx := context.Background()

	page, err := registry.ListPools(ctx, projectA, fleet.ListWindow{Limit: 50})
	if err != nil {
		t.Fatalf("list A's pools: %v", err)
	}
	if len(page) != 1 || page[0].ID != poolA {
		t.Fatalf("tenant A's pools = %v, want exactly %s", page, poolA)
	}
	if page[0].Posture != "unsandboxed-host" {
		t.Fatalf("pool %s posture = %q, want unsandboxed-host — the posture IS the pool", poolA, page[0].Posture)
	}
	if page[0].CreatedAt.IsZero() {
		t.Fatal("a listed pool carries no created_at; the pagination cursor would be minted from the zero time")
	}
	page, err = registry.ListPools(ctx, projectB, fleet.ListWindow{Limit: 50})
	if err != nil {
		t.Fatalf("list B's pools: %v", err)
	}
	if len(page) != 1 || page[0].ID != poolB {
		t.Fatalf("tenant B's pools = %v, want exactly %s", page, poolB)
	}
}
