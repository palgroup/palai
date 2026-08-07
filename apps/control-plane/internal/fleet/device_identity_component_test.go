//go:build component

package fleet_test

// The device-identity proofs against a REAL Postgres (plan §3.4, T2, migration 000007). They live here
// rather than under tests/ for the reason store_component_test.go states — internal/fleet is an internal
// package — and they are component tests rather than unit ones because every property under proof is a
// property of the DATABASE: which row a fingerprint resolves to, what a revoked state refuses, and
// whether two enrolments of one device key can ever produce two rows.
//
// ‼️ THE PACKAGE IS RUN WHOLE. scripts/test/component's postgres suite invokes
// `go test -tags=component -v ./apps/control-plane/internal/fleet` with no -run filter, so a test added
// to this package runs. That was checked rather than assumed: a component test the selector does not
// reach reports the same green as one that passes, which is the trap this tree has fallen into seven
// times.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/device"
	"github.com/palgroup/palai/storage"
)

// deviceEnrolment is one machine's registration carrying a durable device key's fingerprint, which is
// the only difference between this and store_component_test.go's `enrollment`: everything below turns on
// that one column being the thing the registry keys a machine by.
func deviceEnrolment(poolID, label, fingerprint string) fleet.Registration {
	reg := enrollment(poolID, label)
	reg.PublicKeySHA256 = fingerprint
	reg.OS, reg.Arch, reg.Version = "darwin", "arm64", "9.9.9"
	reg.IsolationModes = []string{device.IsolationUser}
	return reg
}

// TestARestartRecoversTheSameRowRatherThanAddingOne is DoD 6 measured against the database.
//
// ‼️ THE DEFECT, MEASURED ON THIS TREE 2026-08-05: `packages/runner.Enroll` generated a fresh keypair on
// every call and `cmd/runner` called it once per PROCESS START, so `runners` grew a row per reboot. The
// fingerprint column existed since 000001 and nothing keyed on it. What is asserted here is the count —
// four enrolments, one row — plus the id, because a store that recovered the row while returning a new id
// would leave the certificate naming a machine no row records.
func TestARestartRecoversTheSameRowRatherThanAddingOne(t *testing.T) {
	pool := openDeviceSpine(t)
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()
	fingerprint := newID("fp")

	first, err := registry.Register(ctx, deviceEnrolment(poolID, "mac-mini-01", fingerprint))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}

	// Three restarts. Each presents the same device key and the id it holds, exactly as an installed
	// agent does — and each one is a DIFFERENT minted id from the gateway, which is what makes the
	// recovery observable: the store must ignore the mint and return the row it already has.
	for i := 0; i < 3; i++ {
		reg := deviceEnrolment(poolID, "mac-mini-01", fingerprint)
		reg.RecoverRunnerID = first.ID
		again, err := registry.Register(ctx, reg)
		if err != nil {
			t.Fatalf("restart %d: %v", i+1, err)
		}
		if again.ID != first.ID {
			t.Fatalf("restart %d came back as %q, want %q", i+1, again.ID, first.ID)
		}
		if again.DNS != first.DNS {
			t.Fatalf("restart %d recovered id %q but DNS %q, want %q: a row whose SAN does not name its own id "+
				"is a row found exactly never", i+1, again.ID, again.DNS, first.DNS)
		}
	}

	if got := countRows(t, pool, `SELECT count(*) FROM runners WHERE pool_id = $1`, poolID); got != 1 {
		t.Fatalf("four starts of one machine produced %d rows, want 1", got)
	}

	// The journal RECORDS each recovery. A machine whose identity was reissued and whose journal says
	// nothing is a certificate no audit can explain.
	entries := countRows(t, pool, `SELECT count(*) FROM runner_enrollments WHERE runner_id = $1`, first.ID)
	if entries != 4 {
		t.Fatalf("the journal holds %d entries for four enrolments, want 4", entries)
	}
	_ = project
}

// TestARecoveredMachineKeepsTheStateAnOperatorGaveIt is the security half of recovery, and it is the leg
// that stops a restart from being a way around two separate operator decisions.
//
// ‼️ A STRICT POOL'S WAITING ROOM MUST SURVIVE A REBOOT. If a re-enrolment wrote `active`, a machine an
// operator had deliberately NOT approved would admit itself by restarting — which is the whole of strict
// enrolment, bypassed by a power cycle. The same argument covers `cordoned`.
func TestARecoveredMachineKeepsTheStateAnOperatorGaveIt(t *testing.T) {
	pool := openDeviceSpine(t)
	_, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := storage.WithSystemScope(context.Background())

	for _, state := range []string{"pending", "cordoned"} {
		fingerprint := newID("fp")
		row, err := registry.Register(context.Background(), deviceEnrolment(poolID, "mac-"+state, fingerprint))
		if err != nil {
			t.Fatalf("enrol: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE runners SET state = $2 WHERE id = $1`, row.ID, state); err != nil {
			t.Fatalf("set state: %v", err)
		}
		reg := deviceEnrolment(poolID, "mac-"+state, fingerprint)
		reg.RecoverRunnerID = row.ID
		recovered, err := registry.Register(context.Background(), reg)
		if err != nil {
			t.Fatalf("recover a %s machine: %v", state, err)
		}
		if recovered.State != state {
			t.Fatalf("a %s machine came back as %q: a restart moved a machine an operator had decided about",
				state, recovered.State)
		}
	}
}

// TestARevokedDeviceCannotRecoverEvenWithALivePoolKey is the third refusal plan §T2 names.
//
// ‼️ A POOL KEY ENROLS A FLEET, NOT A BOX. It is reusable by design, so before device keys "revoke that
// Mac" was undone by the Mac restarting: it came back under a new id with the key still on its disk, and
// the revocation named a row nothing would ever present again. The fingerprint is what the revocation
// actually named.
func TestARevokedDeviceCannotRecoverEvenWithALivePoolKey(t *testing.T) {
	pool := openDeviceSpine(t)
	_, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	fingerprint := newID("fp")

	row, err := registry.Register(context.Background(), deviceEnrolment(poolID, "condemned", fingerprint))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if _, err := pool.Exec(storage.WithSystemScope(context.Background()),
		`UPDATE runners SET state = 'revoked' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	reg := deviceEnrolment(poolID, "condemned", fingerprint)
	reg.RecoverRunnerID = row.ID
	if _, err := registry.Register(context.Background(), reg); !errors.Is(err, fleet.ErrRunnerRevoked) {
		t.Fatalf("re-enrolment of a revoked device returned %v, want ErrRunnerRevoked: a decommissioning the "+
			"machine can undo by restarting is not a decommissioning", err)
	}
	// AND IT DID NOT QUIETLY CREATE A SECOND ROW INSTEAD, which is the failure that would satisfy the
	// error assertion above only if the error came from somewhere else.
	if got := countRows(t, pool, `SELECT count(*) FROM runners WHERE pool_id = $1`, poolID); got != 1 {
		t.Fatalf("the refused re-enrolment left %d rows in the pool, want the one revoked row", got)
	}
}

// TestAClaimedIdentityMustBelongToThisDeviceKey covers the two shapes of a claim the fingerprint cannot
// support, and the second one is the honest re-image rather than the attack.
func TestAClaimedIdentityMustBelongToThisDeviceKey(t *testing.T) {
	pool := openDeviceSpine(t)
	_, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()

	victim, err := registry.Register(ctx, deviceEnrolment(poolID, "victim", newID("fp")))
	if err != nil {
		t.Fatalf("enrol the machine being impersonated: %v", err)
	}

	// SHAPE ONE — a different device key claiming the victim's id.
	theft := deviceEnrolment(poolID, "thief", newID("fp"))
	theft.RecoverRunnerID = victim.ID
	if _, err := registry.Register(ctx, theft); !errors.Is(err, fleet.ErrIdentityNotRecoverable) {
		t.Fatalf("a machine holding another key was given the identity it merely named: %v", err)
	}

	// SHAPE TWO — a claim no row anywhere supports (the re-image that kept identity.json).
	orphan := deviceEnrolment(poolID, "reimaged", newID("fp"))
	orphan.RecoverRunnerID = "rnr_never_existed"
	if _, err := registry.Register(ctx, orphan); !errors.Is(err, fleet.ErrIdentityNotRecoverable) {
		t.Fatalf("a claim no fingerprint supports was admitted: %v", err)
	}

	// AND THE NON-VACUITY HALF: a machine that claims nothing becomes a new machine. Without it, a store
	// that refused every registration would pass both legs above.
	if _, err := registry.Register(ctx, deviceEnrolment(poolID, "genuinely-new", newID("fp"))); err != nil {
		t.Fatalf("a genuinely new install with a new key was refused: %v", err)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM runners WHERE pool_id = $1`, poolID); got != 2 {
		t.Fatalf("the pool holds %d rows, want the victim and the new machine", got)
	}
}

// TestAPoolCanRequireAnIsolationModeTheMachineMeasured is DoD 9 against the column 000007 adds, with the
// two admitting cases that keep it from refusing every deployment alive today.
func TestAPoolCanRequireAnIsolationModeTheMachineMeasured(t *testing.T) {
	pool := openDeviceSpine(t)
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()

	// A pool with NO requirement admits a machine that measured only `user` — this is every pool that
	// exists the day 000007 applies, and the leg that keeps the check from being a fleet-wide outage.
	if _, err := registry.Register(ctx, deviceEnrolment(poolID, "unrestricted", newID("fp"))); err != nil {
		t.Fatalf("a pool with no isolation requirement refused a machine: %v", err)
	}

	// ‼️ THE REQUIREMENT IS WRITTEN BY THE PRODUCTION WRITER, and until 2026-08-07 this test wrote it with
	// a raw `UPDATE runner_pools SET isolation_mode` — a state NO operator could produce. That is what hid
	// the gap for a year: every leg below passed, the refusal was real, journalled and correct, and the
	// column it reads had no INSERT or UPDATE anywhere in the tree, so nothing could ever arm it. A fixture
	// that reaches past the writer proves the mechanism and says nothing about whether anyone can ask for it.
	required, err := registry.CreatePool(ctx, project, fleet.Pool{
		Name: newID("accounts-pool"), Posture: "unsandboxed-host", IsolationMode: device.IsolationAccounts,
	})
	if err != nil {
		t.Fatalf("create a pool that REQUIRES accounts isolation: %v", err)
	}
	// Non-vacuity: the writer must have stored it. A CreatePool that dropped the field would leave a pool
	// with no requirement, and every refusal below would silently become an admission.
	if got := requiredMode(t, pool, required.ID); got != device.IsolationAccounts {
		t.Fatalf("the created pool requires %q, want %q — the write path dropped the field, so nothing below is a statement about isolation",
			got, device.IsolationAccounts)
	}

	// The machine measured `user` only: no palai-agentd, so it cannot give each session its own account.
	if _, err := registry.Register(ctx, deviceEnrolment(required.ID, "no-daemon", newID("fp"))); !errors.Is(err, fleet.ErrIsolationUnsupported) {
		t.Fatalf("a machine with no accounts isolation joined an accounts-only pool: %v", err)
	}
	// The refusal is JOURNALLED. An enrolment that "just fails" leaves an operator with nothing to read.
	if got := countRows(t, pool, `SELECT count(*) FROM runner_enrollments WHERE pool_id = $1 AND entry_kind = 'refused'`, required.ID); got != 1 {
		t.Fatalf("the isolation refusal produced %d journal entries, want 1", got)
	}

	// The same machine, having measured the mode, is admitted.
	admitted := deviceEnrolment(required.ID, "with-daemon", newID("fp"))
	admitted.IsolationModes = []string{device.IsolationAccounts, device.IsolationUser}
	if _, err := registry.Register(ctx, admitted); err != nil {
		t.Fatalf("a machine that measured the pool's mode was refused: %v", err)
	}

	// A machine that measured NOTHING is admitted, which is every runner built before packages/device.
	// Without this leg the check refuses every existing deployment for a mechanism none of them declared.
	//
	// AGAINST THE REQUIRING POOL, and that is the whole leg: on the unrestricted pool above it would be a
	// second copy of the first assertion and would hold for a build with no isolation check at all.
	legacy := deviceEnrolment(required.ID, "pre-device", newID("fp"))
	legacy.IsolationModes = nil
	if _, err := registry.Register(ctx, legacy); err != nil {
		t.Fatalf("a runner too old to measure its isolation modes was refused: %v", err)
	}
}

// TestTheDatabaseRefusesTwoMachinesOnOneDeviceKey is the 000007 unique index measured directly, and it is
// the property the Go recovery path cannot provide on its own.
//
// ‼️ WHAT THE INDEX DECIDES IS THE RACE. fleet.Store resolves the fingerprint and recovers inside its
// transaction, so the ordinary path never reaches this constraint. Two enrolments of one device key
// arriving at two control-plane REPLICAS both miss the SELECT and both INSERT — and without the index
// both succeed, leaving two rows for one key and a machine whose id depends on which reply arrived last.
// A structural invariant beats a well-behaved caller, because the caller is what changes.
func TestTheDatabaseRefusesTwoMachinesOnOneDeviceKey(t *testing.T) {
	pool := openDeviceSpine(t)
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	ctx := storage.WithSystemScope(context.Background())
	fingerprint := newID("fp")

	insert := func(id string) error {
		_, err := pool.Exec(ctx, `INSERT INTO runners (id, project_id, pool_id, label, runner_dns, public_key_sha256, state)
			VALUES ($1,$2,$3,'direct',$4,$5,'active')`,
			id, project, poolID, id+".runners.palai.internal", fingerprint)
		return err
	}
	if err := insert(newID("rnr")); err != nil {
		t.Fatalf("first direct insert: %v", err)
	}
	if err := insert(newID("rnr")); err == nil {
		t.Fatal("the database accepted a second row for one device key in one pool: the recovery path is then " +
			"a convention rather than an invariant, and two replicas racing produce two machines")
	}

	// AND THE PARTIAL PREDICATE IS REAL: rows with NO fingerprint are the pre-device shape and any number
	// of them must coexist. An unpartitioned unique index would refuse the second one, which is every
	// control plane running without a registry-recording issuer.
	blank := func(id string) error {
		_, err := pool.Exec(ctx, `INSERT INTO runners (id, project_id, pool_id, label, runner_dns, public_key_sha256, state)
			VALUES ($1,$2,$3,'legacy',$4,'','active')`, id, project, poolID, id+".runners.palai.internal")
		return err
	}
	if err := blank(newID("rnr")); err != nil {
		t.Fatalf("first fingerprint-less row: %v", err)
	}
	if err := blank(newID("rnr")); err != nil {
		t.Fatalf("a second row with no device key was refused: %v\nthe index must be partial, or every control "+
			"plane without a registry-recording issuer stops enrolling after one machine", err)
	}
}

// openDeviceSpine is openPool AFTER the chain has been applied, and the pairing is deliberate rather than
// convenient.
//
// ‼️ A TEST THAT INHERITS ITS SCHEMA FROM AN EARLIER LEG IS A TEST THAT REPORTS WHATEVER THE HARNESS DID.
// This package's capacity suite records the same lesson in its own words: under the tier's PALAI_SUITE_PKG
// selector no earlier leg runs, and the first RED is `relation "projects" does not exist` — a failure that
// names the fixture and says nothing about the property. That is exactly what these six tests did on their
// first run (2026-08-05), which is how this function came to exist. Owning the condition means the suite
// measures the same thing whether it runs alone or last.
func openDeviceSpine(t *testing.T) *pgxpool.Pool {
	t.Helper()
	openSpine(t) // migrates the chain, including 000007, against this container
	return openPool(t)
}

// countRows runs a scalar count under the system scope. It is a helper rather than four copies because
// every one of them would otherwise have to remember the scope, and a query that forgets it returns zero
// for the wrong reason — a green that means "RLS hid the rows".
func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return n
}

// requiredMode reads what a pool ASKS OF A MACHINE, straight from the column, because the claim above is
// about what the write path stored and a read that went back through the same struct could agree with a
// writer that dropped the field.
func requiredMode(t *testing.T, pool *pgxpool.Pool, poolID string) string {
	t.Helper()
	var mode string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT isolation_mode FROM runner_pools WHERE id = $1`, poolID).Scan(&mode); err != nil {
		t.Fatalf("read pool %s isolation_mode: %v", poolID, err)
	}
	return mode
}
