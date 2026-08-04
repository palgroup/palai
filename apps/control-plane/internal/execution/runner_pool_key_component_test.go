//go:build component

package execution_test

// THE POOL ENROLMENT KEY, ON THE REAL WIRE AND AGAINST A REAL POSTGRES (E24 T3).
//
// Every claim here is a claim about a CREDENTIAL, so none of them can be proved against a fake: what
// is under proof is that a row in `runner_pool_keys` decides which pool a certificate is minted into,
// that revoking that row stops the NEXT machine and no machine already holding a certificate, and
// that the same value is worth nothing at all under /v1. A fake store would be asserting its own
// author's beliefs about all three.
//
// WHAT THIS TASK ADDS IS NOT REUSABILITY. The tree moved to a reusable enrolment credential long ago
// and denies it in four places; `FileEnrollmentTokens`' own heading reads "WHY THIS IS NOT ONE-USE,
// AND WHAT REPLACED THAT". What is new is SCOPE, HASH, EXPIRY, REVOCATION and a RECORD.
//
// It is wired into scripts/test/component's postgres suite by the `TestPoolKey` alternative — every
// test in this file carries that prefix deliberately, because a component test the selector does not
// name never runs and reports the same green as one that passes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/storage"
)

// poolKeyPool opens the shared component database. The migrations are applied by the tier's first leg
// (tests/component/postgres), the same assumption internal/fleet's own component tests make.
func poolKeyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	pool, err := storage.OpenPool(context.Background(), url)
	if err != nil {
		t.Fatalf("open component pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func poolKeyID(prefix string) string { return middleware.NewID(prefix) }

// poolKeyTenant seeds an organization, a project and one pool per posture asked for, and returns the
// tenant plus the pool ids in the order the postures were given. Two pools in ONE tenant is the shape
// every claim here needs: a key scoped to one of them must be worth nothing at the other.
func poolKeyTenant(t *testing.T, pool *pgxpool.Pool, postures ...string) (org, project string, poolIDs []string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	org, project = poolKeyID("org"), poolKeyID("prj")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, org)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	for _, posture := range postures {
		id := poolKeyID("pool")
		exec(`INSERT INTO runner_pools (id, organization_id, project_id, name, posture) VALUES ($1,$2,$3,$4,$5)`,
			id, org, project, poolKeyID("name"), posture)
		poolIDs = append(poolIDs, id)
	}
	return org, project, poolIDs
}

// mintPoolKey mints a key for a pool through the PRODUCTION path and returns its id and its one-time
// value. It goes through fleet.PoolEnrollmentKeys rather than an INSERT because the value has to cross
// a boundary and come back — the mint hashes it, the wire presents it, the store resolves the hash —
// and a fake that hashed it the same way the store does would reproduce whatever the store gets wrong.
func mintPoolKey(t *testing.T, keys *fleet.PoolEnrollmentKeys, org, project, poolID string, expiresAt *time.Time) (id, value string) {
	t.Helper()
	minted, err := keys.Mint(context.Background(), org, project, poolID, expiresAt)
	if err != nil {
		t.Fatalf("mint pool key: %v", err)
	}
	if minted.Value == "" {
		t.Fatal("mint returned no key value; the value is shown exactly once and this was that once")
	}
	return minted.ID, minted.Value
}

// poolKeyFixture is the production wiring: a gateway whose credential chain is a pool key FIRST and
// the file bootstrap token SECOND, over a real registry on a real database.
type poolKeyFixture struct {
	*gatewayFixture
	pool     *pgxpool.Pool
	keys     *fleet.PoolEnrollmentKeys
	registry *fleet.Store
	// spine is the coordinator store the capacity waker and the park reaper share (E24 T5). Set only by
	// newPlacementFixture, which is the fixture that migrates and wires it.
	spine *coordinator.Store
}

func newPoolKeyFixture(t *testing.T) *poolKeyFixture {
	t.Helper()
	pool := poolKeyPool(t)
	registry := fleet.NewStore(pool, poolKeyID, nil)
	keys := fleet.NewPoolEnrollmentKeys(pool, poolKeyID, nil)
	// The file token stays in the chain: it is the only rescue path for an expired identity, so the
	// fixture carries one the pool-key tests never present (a distinct value, so a pool key that
	// accidentally matched it would be a failure rather than a pass).
	f := newGatewayFixture(t, newOneUseTokens("pool-key-file-token-never-presented"))
	f.gateway.SetRegistry(registry)
	f.gateway.SetPoolKeys(keys)
	return &poolKeyFixture{gatewayFixture: f, pool: pool, keys: keys, registry: registry}
}

// enrolAsking enrols a machine presenting `key`, DECLARING the pool it believes it belongs to. An
// empty declaration inherits the key's pool, which is what an unconfigured machine sends.
func (f *poolKeyFixture) enrolAsking(ctx context.Context, key, declaredPool string) (runner.Identity, error) {
	config := f.bootstrap(key)
	config.PoolID = declaredPool
	// A distinct local name per call so two machines in one test are two machines: the label is what
	// the machine calls itself and the gateway mints the identity, so this only has to be plausible.
	config.RunnerID = poolKeyID("label")
	config.RunnerDNS = config.RunnerID + ".runners.palai.test"
	return runner.Enroll(ctx, config)
}

// journalEntries reads the enrolment journal for one key, newest first, as (kind, detail) pairs.
func journalEntries(t *testing.T, pool *pgxpool.Pool, keyID string) []struct {
	Kind   string
	Detail map[string]any
} {
	t.Helper()
	rows, err := pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT entry_kind, detail FROM runner_enrollments WHERE key_id = $1 ORDER BY created_at DESC, entry_seq DESC`, keyID)
	if err != nil {
		t.Fatalf("read the enrolment journal: %v", err)
	}
	defer rows.Close()
	out := []struct {
		Kind   string
		Detail map[string]any
	}{}
	for rows.Next() {
		var kind string
		var raw []byte
		if err := rows.Scan(&kind, &raw); err != nil {
			t.Fatalf("scan journal entry: %v", err)
		}
		detail := map[string]any{}
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode journal detail: %v", err)
		}
		out = append(out, struct {
			Kind   string
			Detail map[string]any
		}{kind, detail})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the journal: %v", err)
	}
	return out
}

// TestPoolKeyEnrolsOnlyIntoThePoolItNames is RED (1), SCOPE: a key minted for the Mac pool admits into
// the Mac pool and REFUSES a machine that declares the Linux pool — and the refusal is JOURNALLED, so
// an operator who pasted the wrong key onto a machine has something to read instead of a machine that
// silently joined the wrong fleet.
//
// The proof is two-sided on purpose: a credential that refused everything would satisfy the refusal
// half on its own, so the SAME key is shown enrolling into the pool it names, in the same test.
func TestPoolKeyEnrolsOnlyIntoThePoolItNames(t *testing.T) {
	f := newPoolKeyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	org, project, pools := poolKeyTenant(t, f.pool, "unsandboxed-host", "sandboxed-linux")
	macPool, linuxPool := pools[0], pools[1]
	macKeyID, macKey := mintPoolKey(t, f.keys, org, project, macPool, nil)

	// The key admits into the pool it names.
	identity, err := f.enrolAsking(ctx, macKey, macPool)
	if err != nil {
		t.Fatalf("the Mac pool's key did not admit a machine into the Mac pool: %v", err)
	}
	var landedPool, viaKey string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT pool_id, coalesce(enrolled_via_key_id,'') FROM runners WHERE runner_dns = $1`,
		identity.RunnerID+".runners.palai.internal").Scan(&landedPool, &viaKey); err != nil {
		t.Fatalf("the enrolled machine is not in the registry: %v", err)
	}
	if landedPool != macPool {
		t.Fatalf("the machine landed in pool %q, want the key's %q", landedPool, macPool)
	}
	// R3: WHICH key minted this identity. It is the one fact targeted revocation needs and the one
	// fact the tree did not record (§3.6 D5).
	if viaKey != macKeyID {
		t.Fatalf("runners.enrolled_via_key_id = %q, want the key that minted it %q", viaKey, macKeyID)
	}

	// The same key, declaring the OTHER pool: refused.
	if _, err := f.enrolAsking(ctx, macKey, linuxPool); err == nil {
		t.Fatal("the Mac pool's key enrolled a machine that asked for the Linux pool")
	}
	// And the refusal is a record, not just a status code.
	refusals := 0
	for _, entry := range journalEntries(t, f.pool, macKeyID) {
		if entry.Kind != "refused" {
			continue
		}
		refusals++
		if entry.Detail["reason"] != "pool_scope_mismatch" {
			t.Fatalf("refusal reason = %v, want pool_scope_mismatch", entry.Detail["reason"])
		}
		if entry.Detail["declared_pool"] != linuxPool || entry.Detail["key_pool"] != macPool {
			t.Fatalf("refusal detail = %v, want the pool declared (%s) and the pool the key admits into (%s)",
				entry.Detail, linuxPool, macPool)
		}
	}
	if refusals != 1 {
		t.Fatalf("runner_enrollments holds %d refused entries for this key, want exactly 1", refusals)
	}
}

// TestPoolKeyRevocationRefusesANewEnrolment is RED (2)(a): after the key is revoked, the next machine
// is turned away — and the refusal is journalled with the key that was presented.
//
// Its non-vacuity is the first leg: the same key, the same machine shape, enrolling successfully one
// statement earlier. A key that never worked would satisfy the refusal for the wrong reason.
func TestPoolKeyRevocationRefusesANewEnrolment(t *testing.T) {
	f := newPoolKeyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	org, project, pools := poolKeyTenant(t, f.pool, "sandboxed-linux")
	keyID, key := mintPoolKey(t, f.keys, org, project, pools[0], nil)

	if _, err := f.enrolAsking(ctx, key, ""); err != nil {
		t.Fatalf("the key did not work before it was revoked: %v", err)
	}
	revoked, err := f.keys.Revoke(ctx, org, project, keyID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// THE OPERATOR IS SHOWN WHAT THEY DID NOT STOP. Revocation closes the door and leaves the machines
	// that came through it running, so the revoke answer has to name them — otherwise "revoked" reads
	// as "removed" and an operator believes a fleet was decommissioned by one call.
	if len(revoked.EnrolledRunners) != 1 {
		t.Fatalf("revoke reported %d machine(s) enrolled with this key, want 1", len(revoked.EnrolledRunners))
	}

	if _, err := f.enrolAsking(ctx, key, ""); err == nil {
		t.Fatal("a revoked pool key still minted a certificate")
	}
	refusals := 0
	for _, entry := range journalEntries(t, f.pool, keyID) {
		if entry.Kind == "refused" && entry.Detail["reason"] == "key_revoked" {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("runner_enrollments holds %d key_revoked refusals, want exactly 1", refusals)
	}
}

// TestPoolKeyRevocationLeavesAnEnrolledRunnerRenewing is RED (2)(b) and it is a REGRESSION FENCE, not
// a feature: renewal already survives revocation and STAYING that way is this epic's promise. The
// structural reason is worth stating where it is measured — `handleRenew` authenticates with the
// certificate the machine already holds and the credential chain is not on that path at all (§3.6 D5),
// so a key that no longer admits anyone cannot unmake an identity it already minted.
//
// A fleet where revoking one pasted-around key stopped every machine that had ever used it would be a
// fleet nobody dares revoke a key in.
func TestPoolKeyRevocationLeavesAnEnrolledRunnerRenewing(t *testing.T) {
	f := newPoolKeyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	org, project, pools := poolKeyTenant(t, f.pool, "sandboxed-linux")
	keyID, key := mintPoolKey(t, f.keys, org, project, pools[0], nil)

	identity, err := f.enrolAsking(ctx, key, pools[0])
	if err != nil {
		t.Fatalf("enrol with the pool key: %v", err)
	}
	if _, err := f.keys.Revoke(ctx, org, project, keyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The machine rolls its identity forward twice AFTER the revocation, over its existing mTLS
	// identity. Twice rather than once because a renewal that worked only from the enrolment
	// certificate would still be a fleet that dies one lifetime later.
	current := identity
	for round := 1; round <= 2; round++ {
		renewed, err := runner.Renew(ctx, current, f.renewConfig())
		if err != nil {
			t.Fatalf("renew %d after the key was revoked: %v", round, err)
		}
		if renewed.RunnerID != identity.RunnerID {
			t.Fatalf("renewal changed the machine's identity: %q -> %q", identity.RunnerID, renewed.RunnerID)
		}
		if renewed.Certificate.Leaf.SerialNumber.Cmp(current.Certificate.Leaf.SerialNumber) == 0 {
			t.Fatal("renewal returned the same certificate, not a fresh issuance")
		}
		current = renewed
	}
	// The row is still the row, still bound to the revoked key: revocation records nothing about the
	// machines that already enrolled, which is exactly why the operator has to be shown them.
	var state, viaKey string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT state, coalesce(enrolled_via_key_id,'') FROM runners WHERE runner_dns = $1`,
		identity.RunnerID+".runners.palai.internal").Scan(&state, &viaKey); err != nil {
		t.Fatalf("read the enrolled machine: %v", err)
	}
	if state != "active" || viaKey != keyID {
		t.Fatalf("after revoking its key the machine is (state=%q, via=%q), want (active, %s)", state, viaKey, keyID)
	}
}

// TestPoolKeyRevocationDoesNotCutAnInFlightLease is RED (2)(c): the third half. A machine holding a
// LEASE when its enrolment key is revoked keeps serving that lease — frames still relay both ways.
// Cutting work in flight is `Revoke()` on the gateway (SAN-011, T5's surface), and a key revocation
// must not be a back door into it.
func TestPoolKeyRevocationDoesNotCutAnInFlightLease(t *testing.T) {
	f := newPoolKeyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	org, project, pools := poolKeyTenant(t, f.pool, "sandboxed-linux")
	keyID, key := mintPoolKey(t, f.keys, org, project, pools[0], nil)

	identity, err := f.enrolAsking(ctx, key, pools[0])
	if err != nil {
		t.Fatalf("enrol with the pool key: %v", err)
	}
	sessCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	leaseReady := make(chan *runner.LeaseSession, 1)
	go func() {
		if ls, err := f.session(identity).OpenLease(sessCtx); err == nil {
			leaseReady <- ls
		}
	}()
	// The attempt names the pool the machine enrolled into AND the tenant that pool belongs to — a Dial
	// on any other pool would not reach it at all (T2's refusal), and since E24 T4 neither would a Dial
	// that named no tenant, because the rendezvous is keyed by both. Neither is this test's subject.
	attempt := f.attempt("run_poolkeylease", "att_poolkeylease", 4)
	attempt.PoolID = pools[0]
	attempt.Tenant = coordinator.Tenant{Project: project}
	ch, err := f.gateway.Dial(ctx, attempt)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer ch.Close()
	var lease *runner.LeaseSession
	select {
	case lease = <-leaseReady:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner never obtained the lease")
	}

	// Revoke the key WHILE the lease is in flight.
	if _, err := f.keys.Revoke(ctx, org, project, keyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The machine's next frame still reaches the attempt.
	frame := contracts.EngineFrame{
		Protocol: "engine.v1", ID: "frm_poolkey1", Type: "engine.ready", Sequence: 1,
		Time: time.Now().UTC().Format(time.RFC3339), RunID: "run_poolkeylease",
	}
	if err := lease.SendEngineFrame(ctx, frame); err != nil {
		t.Fatalf("the runner could not send a frame after its key was revoked: %v", err)
	}
	relayed, err := ch.Receive(ctx)
	if err != nil {
		t.Fatalf("the in-flight lease was cut by a key revocation: %v", err)
	}
	if relayed.ID != frame.ID {
		t.Fatalf("relayed frame = %q, want the frame the runner sent %q", relayed.ID, frame.ID)
	}
}

// TestPoolKeyIsNotAnAPIKey is RED (3): a pool key is worth NOTHING under /v1. It carries no scope, it
// is not in `api_keys`, and `Scope.HasScope`'s empty-scope-means-every-capability rule
// (api/middleware/auth.go:30) therefore cannot be reached with one.
//
// It is a FENCE and it is honest about that: nothing in the tree resolved a pool key before this task,
// so the 401 was already true. What makes it non-vacuous is the pairing — the SAME route, the SAME
// server, answered 200 for a real API key in the same test — plus the assertion that the pool key's
// digest appears in no `api_keys` row, which is the only structural reason the 401 holds.
func TestPoolKeyIsNotAnAPIKey(t *testing.T) {
	f := newPoolKeyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	org, project, pools := poolKeyTenant(t, f.pool, "sandboxed-linux")
	_, key := mintPoolKey(t, f.keys, org, project, pools[0], nil)

	// A real API key for the same tenant, minted the way the tenancy surface mints one. It carries
	// `system` because GET /v1/runners now sits behind the fleet's systemOnly gate (Faz A.1 Task 3),
	// which — unlike HasScope — never treats an empty `scopes` set as unrestricted; without it this key
	// would 403 at that gate and the pairing below would prove nothing about the pool key at all.
	apiKey := "sk_poolkeyfence_" + hex.EncodeToString([]byte(poolKeyID("x")))
	sys := storage.WithSystemScope(ctx)
	for _, stmt := range [][]any{
		{`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1,$2,$3,'service')`, "prin_pkf_" + org, org, project},
		{`INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash, scopes)
		  VALUES ($1,$2,$3,$4,$5,$6)`, "key_pkf_" + org, org, project, "prin_pkf_" + org, coordinator.HashAPIKey(apiKey), []string{middleware.ScopeSystem}},
	} {
		if _, err := f.pool.Exec(sys, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed the API key: %v", err)
		}
	}

	// The SHIPPED router over a REAL verifier, mounting a real /v1 route (the runner read surface, so
	// the route a pool key would most plausibly be aimed at is the one under test).
	ts := poolKeyRouter(t, fleet.NewRegistryAPI(f.registry, f.keys))
	poolKeyStatus, poolKeyBody := poolKeyGet(t, ts, key)
	if poolKeyStatus != http.StatusUnauthorized {
		t.Fatalf("GET /v1/runners with a POOL KEY = %d, want 401: %s", poolKeyStatus, poolKeyBody)
	}
	var problem struct{ Code string }
	_ = json.Unmarshal(poolKeyBody, &problem)
	if problem.Code != "invalid_token" {
		t.Fatalf("pool key rejection code = %q, want invalid_token (it resolved no scope at all)", problem.Code)
	}
	// The pairing: the same route, the same server, a credential that IS an API key.
	apiStatus, apiBody := poolKeyGet(t, ts, apiKey)
	if apiStatus != http.StatusOK {
		t.Fatalf("GET /v1/runners with a real API key = %d, want 200 — the 401 above must be about the credential, not the route: %s", apiStatus, apiBody)
	}
	// The structural reason, asserted rather than argued: the key's digest is in no api_keys row, so
	// there is no row for VerifyAPIKey to find and no scope for HasScope to widen.
	digest := sha256.Sum256([]byte(key))
	var inAPIKeys int
	if err := f.pool.QueryRow(sys, `SELECT count(*) FROM api_keys WHERE key_hash = $1`, hex.EncodeToString(digest[:])).Scan(&inAPIKeys); err != nil {
		t.Fatalf("count api_keys rows for the pool key digest: %v", err)
	}
	if inAPIKeys != 0 {
		t.Fatalf("the pool key's digest is in %d api_keys row(s); a pool key that is an API key carries every capability", inAPIKeys)
	}
}

// poolKeyRouter serves the shipped public router over a REAL VerifyAPIKey against this database.
func poolKeyRouter(t *testing.T, registry api.RunnerRegistryAPI) *httptest.Server {
	t.Helper()
	repo, err := store.Open(context.Background(), os.Getenv("PALAI_COMPONENT_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("open the repository for a real verifier: %v", err)
	}
	t.Cleanup(repo.Close)
	ts := httptest.NewServer(api.NewRouter(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithRunners(registry)))
	t.Cleanup(ts.Close)
	return ts
}

func poolKeyGet(t *testing.T, ts *httptest.Server, bearer string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/runners", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /v1/runners: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestPoolKeyValueReachesNoRowJournalOrLog is the sweep: after a key has been minted, used, refused,
// revoked and used again, the VALUE appears nowhere but the one stdout line that showed it.
//
// IT DECODES RATHER THAN GREPS, and that is the whole method. A raw-byte scan over compressed or
// encoded output can never fail — this repository shipped exactly that mistake once, a
// no-secret-in-bundle assertion over gzip bytes that deflate had bit-packed — so every JSONB detail is
// DECODED and every string inside it, at any depth, is compared. The row columns are read as text and
// the gateway's log stream is captured, so a value that reached a log line is a failure here.
func TestPoolKeyValueReachesNoRowJournalOrLog(t *testing.T) {
	// The tree logs through the standard logger (internal/execution uses log.Printf throughout), so
	// pointing it at a buffer for the duration captures anything any path here writes.
	logs := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	f := newPoolKeyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	org, project, pools := poolKeyTenant(t, f.pool, "unsandboxed-host", "sandboxed-linux")
	keyID, key := mintPoolKey(t, f.keys, org, project, pools[0], nil)

	// Everything a key can be part of: an enrolment, a scope refusal, a revocation, a refused enrolment.
	if _, err := f.enrolAsking(ctx, key, pools[0]); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	_, _ = f.enrolAsking(ctx, key, pools[1])
	if _, err := f.keys.Revoke(ctx, org, project, keyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, _ = f.enrolAsking(ctx, key, pools[0])
	// The listing an operator reads. It carries the prefix, which is a prefix and not the credential.
	listed, err := f.keys.List(ctx, org, project, pools[0])
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("list returned %d keys, want 1", len(listed))
	}
	if listed[0].Prefix != key[:8] {
		t.Fatalf("listed prefix = %q, want the value's first 8 characters", listed[0].Prefix)
	}

	haystacks := map[string]string{}
	// Every column of every row in the three tables a key touches, rendered as text by the database
	// itself, so a column added later is swept without this test being edited.
	for _, table := range []string{"runner_pool_keys", "runners", "runner_enrollments"} {
		rows, err := f.pool.Query(storage.WithSystemScope(ctx),
			fmt.Sprintf(`SELECT to_jsonb(t)::text FROM %s t`, table)) // table names are literals above
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		i := 0
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			haystacks[fmt.Sprintf("%s row %d", table, i)] = text
			i++
			// The DECODE half: the JSONB detail is a structured document, and a credential hidden in a
			// nested field would survive a scan of the columns' outer text if any encoding intervened.
			var row map[string]any
			if err := json.Unmarshal([]byte(text), &row); err != nil {
				rows.Close()
				t.Fatalf("decode %s row: %v", table, err)
			}
			for path, leaf := range flattenStrings(row, table) {
				haystacks[path] = leaf
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s: %v", table, err)
		}
	}
	haystacks["gateway log"] = logs.String()
	// The listing projection an operator (and the CLI) actually sees.
	rendered, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("marshal the listing: %v", err)
	}
	haystacks["key listing"] = string(rendered)

	for where, hay := range haystacks {
		if hay == "" {
			continue
		}
		if strings.Contains(hay, key) {
			t.Fatalf("the pool key VALUE is in %s", where)
		}
		// The prefix is published deliberately; the rest of the value is what must never appear, and a
		// truncated leak is still a leak. Nine characters is one past the published prefix.
		if len(key) > 9 && strings.Contains(hay, key[:9]) && where != "key listing" {
			t.Fatalf("%s carries more of the key than the published 8-character prefix", where)
		}
	}
	// NON-VACUITY: the sweep must be able to fail. The same walk over a document that DOES carry the
	// value finds it.
	planted, _ := json.Marshal(map[string]any{"detail": map[string]any{"nested": key}})
	found := false
	var doc map[string]any
	_ = json.Unmarshal(planted, &doc)
	for _, leaf := range flattenStrings(doc, "planted") {
		if strings.Contains(leaf, key) {
			found = true
		}
	}
	if !found {
		t.Fatal("the sweep cannot find a value it was handed; it proves nothing about the values it did not find")
	}
}

// flattenStrings walks a decoded JSON document and returns every string leaf by path. It is what makes
// the sweep a DECODE rather than a grep: a value nested inside a JSONB detail, or inside a string that
// itself holds JSON, is compared as a value rather than hoped to survive a byte scan.
func flattenStrings(node any, path string) map[string]string {
	out := map[string]string{}
	switch v := node.(type) {
	case string:
		out[path] = v
		// A string that is itself a JSON document is decoded one level further: the journal's detail
		// arrives as JSONB, and an encoded-inside-encoded value is exactly what a byte scan misses.
		var nested any
		if err := json.Unmarshal([]byte(v), &nested); err == nil {
			if _, plain := nested.(string); !plain {
				for k, leaf := range flattenStrings(nested, path+".json") {
					out[k] = leaf
				}
			}
		}
	case map[string]any:
		for key, child := range v {
			for k, leaf := range flattenStrings(child, path+"."+key) {
				out[k] = leaf
			}
		}
	case []any:
		for i, child := range v {
			for k, leaf := range flattenStrings(child, fmt.Sprintf("%s[%d]", path, i)) {
				out[k] = leaf
			}
		}
	}
	return out
}

// lockedBuffer is a concurrency-safe io.Writer for the captured log stream: the gateway writes from
// request goroutines while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
