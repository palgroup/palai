//go:build component

package fleet_test

// WHAT A MACHINE THAT DECLARED NOTHING HOLDS (Faz A.4 T5).
//
// The ceiling itself is proved in tests/component/fleet, over machines whose capacity the fixture writes.
// This one is here for the reason store_component_test.go is here: it drives ENROLMENT, and enrolment is
// `fleet.Store.Register`, which Go forbids importing from outside apps/control-plane. A test that inserted
// its own runner row could assert what a stored 1 does; only this one can assert where the 1 comes from.
//
// WHAT CHANGED ON 2026-08-05, because this file asserted the OPPOSITE that morning. Measured then:
//
//	grep -rn 'Registration{' --include='*.go' . | grep -v node_modules
//	→ runner_gateway.go (the one production site) + this package's own test helpers
//
// and that site set ID, PoolID, Label, DNS, PublicKeySHA256, OS, Arch, Posture and KeyID — not Capacity.
// `packages/runner` never said the word, and storage/queries/runners.sql had no UPDATE. So `reg.Capacity`
// was 0 on every enrolment there had ever been, store.go clamped it to 1, and the live stack held forty-four
// machines with one distinct value.
//
// The first version of this test read that 1 as a declaration and asserted a machine declaring nothing
// holds exactly one. That was ENFORCING AN ARTEFACT: nobody chose the number, no operator could change it,
// and a ceiling built on it capped every Mac at a single session on the strength of a clamp. This tree's own
// rule is that overstating a ceiling is as wrong as understating it — it leaves the operator a demonstration
// that does not work when they run it.
//
// SO THE ASSERTION IS INVERTED AND STRONGER. 000005_capacity_declaration made zero storable and moved the
// default to it, the clamp is gone, and the enrolment request carries `capacity` — so zero now means the
// machine DECLARED NOTHING, and an undeclared machine is NOT capped. The two wrong answers this catches are
// opposite: a ceiling reading zero as a literal bound refuses EVERY session (`count(*) < 0` is never true),
// and one that hard-codes a 1 caps a machine that asked for nothing. The three-capacity machine in
// tests/component/fleet catches a build that ignores the column entirely; neither test catches the other's.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// openSpine opens the coordinator store over the same database openPool serves, because the acquire under
// test lives there and the enrolment under test lives here. Both halves are needed to say anything: the
// registry decides what capacity is STORED and the spine decides what the stored number DOES.
//
// IT MIGRATES, AND THAT LINE IS NOT BOILERPLATE — IT IS THIS TEST OWNING ITS OWN PRECONDITION. The rest of
// this package's component tests take the schema as given, because scripts/test/component runs the
// ./tests/component/postgres leg against the same container first. Under the tier's own PALAI_SUITE_PKG
// selector no earlier leg runs, and the first RED of this test was `relation "projects" does not exist` —
// a failure that names the fixture and says nothing about capacity. A test that inherits a condition
// somebody else supplies is a test that reports whatever the harness happens to have done, which is a
// shape this tree has now recorded on both sides: a green that belonged to the harness, and this red.
func openSpine(t *testing.T) *coordinator.Store {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	cs, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

// TestAMachineThatDeclaredNothingIsNotCapped — AN ABSENT DECLARATION IS NOT A CEILING OF ANY SIZE.
//
// A machine enrols the way every machine in every deployment enrols today: saying nothing about how much it
// can hold. It must store ZERO — the value that means "declared nothing" — and it must then take session
// after session, because there is no number to hold it to.
//
// THIS IS THE GUARD ON TODAY'S BEHAVIOUR, which is what makes it worth more than the assertion it replaced.
// The ceiling shipped in the same phase as this test; if it ever bound a machine that declared nothing, every
// Mac in every deployment would cap at whatever the clamp of the week happened to be, and the first symptom
// would be a customer's second session failing. Four holds rather than two, because a fixture that stopped at
// two could not tell "no ceiling" from "a ceiling of two".
func TestAMachineThatDeclaredNothingIsNotCapped(t *testing.T) {
	pool := openPool(t)
	spine := openSpine(t) // migrates, so the fixture below has tables to write into
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()
	tenant := coordinator.Tenant{Project: project}

	declaring := enrollment(poolID, "runner-local")
	// NOT SET, and that absence is the subject: the production enrolment site sends `capacity` only when an
	// operator configured one, and `omitempty` keeps it off the wire otherwise.
	if declaring.Capacity != 0 {
		t.Fatalf("the fixture declared capacity %d — this test is about a machine that declares NOTHING", declaring.Capacity)
	}
	machine, err := registry.Register(ctx, declaring)
	if err != nil {
		t.Fatalf("register a machine that declares no capacity: %v", err)
	}
	// STORED AS ZERO, NOT CLAMPED TO ONE. This is the half that could regress silently: a clamp restored
	// anywhere on the enrolment path would write a number nobody chose, and every assertion below would then
	// be measuring a ceiling of 1 rather than the absence of one.
	if machine.Capacity != 0 {
		t.Fatalf("a machine that declared nothing was stored with capacity %d, want 0 — something on the enrolment path is inventing a declaration, and the ceiling would then enforce it", machine.Capacity)
	}

	seedSession := func() string {
		t.Helper()
		id := newID("ses")
		if _, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO sessions (id, project_id) VALUES ($1, $2)`, id, project); err != nil {
			t.Fatalf("seed a session: %v", err)
		}
		return id
	}

	for i := 1; i <= 4; i++ {
		if _, err := spine.AcquireLease(ctx, tenant, seedSession(), machine.ID); err != nil {
			t.Fatalf("hold %d on a machine that declared NOTHING = %v, want success — an absent declaration is being read as a ceiling, so every machine in every deployment caps at a number no operator chose", i, err)
		}
	}

	// AND A MACHINE THAT DOES DECLARE ONE IS STILL HELD TO IT. Without this the test above is satisfied by a
	// build that simply deleted the ceiling, which is the other way to make an undeclared machine uncapped.
	capped, err := registry.Register(ctx, declaringCapacity(poolID, 1))
	if err != nil {
		t.Fatalf("register a machine declaring capacity 1: %v", err)
	}
	if capped.Capacity != 1 {
		t.Fatalf("a machine that declared 1 was stored with capacity %d — the declaration is not reaching the row", capped.Capacity)
	}
	if _, err := spine.AcquireLease(ctx, tenant, seedSession(), capped.ID); err != nil {
		t.Fatalf("the first hold on a machine declaring 1 = %v, want success", err)
	}
	if _, err := spine.AcquireLease(ctx, tenant, seedSession(), capped.ID); !errors.Is(err, coordinator.ErrMachineAtCapacity) {
		t.Fatalf("the second hold on a machine declaring 1 = %v, want ErrMachineAtCapacity — the ceiling is gone entirely, so the uncapped machine above proved nothing", err)
	}
}

// declaringCapacity is `enrollment` with a number said out loud — the opt-in half, and the only way any row
// in this tree gets a capacity that is not zero.
func declaringCapacity(poolID string, capacity int) fleet.Registration {
	reg := enrollment(poolID, "runner-local")
	reg.Capacity = capacity
	return reg
}

// TestADeclaredFullMachineRefusesTheHoldWithoutKillingTheAttempt — THE REFUSAL IS AN ANSWER, AND TODAY IT
// DELIBERATELY DOES NOT DECIDE THE RUN'S FATE.
//
// Both halves are asserted because both are ceilings, and a ceiling is claimed rather than assumed. The hold
// IS refused, under its own name, and the machine never goes over what it declared. And the attempt is NOT
// killed: holdMachine has no error result at all, so a capacity refusal cannot end a run.
//
// WHY NOT KILL IT, which is the half a reader will push on. Two things have to land first and neither has:
// nothing wakes a run when a slot FREES (the only capacity wake fires when a machine CONNECTS), and open
// occupancies still LEAK — the idle releaser is the only thing that closes one and it only sees a workspace
// that returned to `ready`, which on the live stack was ZERO of 84. A ceiling that ended attempts on top of
// a leak is a run killer with a delay fuse: a declared fleet would lose a slot at a time, permanently, and
// begin refusing real work for a reason nobody could see. Overstating a ceiling is as expensive in this tree
// as understating one.
//
// THE SIGNATURE IS THE PROOF FOR THE SECOND HALF, not a comment. `holdMachine` returning a bare string means
// there is no channel through which this refusal could fail an attempt — checked below by reading the
// declaration, because a future edit that adds an error result is exactly the change this pins.
func TestADeclaredFullMachineRefusesTheHoldWithoutKillingTheAttempt(t *testing.T) {
	pool := openPool(t)
	spine := openSpine(t)
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()
	tenant := coordinator.Tenant{Project: project}

	machine, err := registry.Register(ctx, declaringCapacity(poolID, 1))
	if err != nil {
		t.Fatalf("register a machine declaring capacity 1: %v", err)
	}
	seedSession := func() string {
		t.Helper()
		id := newID("ses")
		if _, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO sessions (id, project_id) VALUES ($1, $2)`, id, project); err != nil {
			t.Fatalf("seed a session: %v", err)
		}
		return id
	}

	if _, err := spine.AcquireLease(ctx, tenant, seedSession(), machine.ID); err != nil {
		t.Fatalf("the first hold = %v, want success", err)
	}
	refused := seedSession()
	if _, err := spine.AcquireLease(ctx, tenant, refused, machine.ID); !errors.Is(err, coordinator.ErrMachineAtCapacity) {
		t.Fatalf("the second hold on a machine declaring 1 = %v, want ErrMachineAtCapacity", err)
	}

	// THE REFUSED SESSION OWNS NO OCCUPANCY, which is the assertion that distinguishes "refused" from
	// "refused and let through anyway": a build that ran the attempt unmetered leaves no row either, so the
	// row count alone cannot tell them apart — but a build that ran it and DID open a row would be over the
	// declared ceiling, and that is what this catches.
	held, err := spine.SessionOccupancies(ctx, tenant, refused)
	if err != nil {
		t.Fatalf("read the refused session's occupancies: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("the refused session owns %d occupancy row(s), want 0 — the machine took a session past the capacity it declared", len(held))
	}
	var open int
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM runner_leases WHERE runner_id = $1 AND released_at IS NULL`, machine.ID).Scan(&open); err != nil {
		t.Fatalf("count the machine's open holds: %v", err)
	}
	if open != 1 {
		t.Fatalf("%d open hold(s) on a machine that declared 1 — the ceiling did not hold", open)
	}

	// AND THE ATTEMPT IS NOT KILLED. Read off the declaration rather than asserted in prose: if
	// `holdMachine` ever grows an error result, a refusal gains a path to end a run, and that must not
	// happen until the leak and the waker are both closed.
	assertHoldMachineCannotFailAnAttempt(t)
}

// assertHoldMachineCannotFailAnAttempt parses the orchestrator's own source and requires that holdMachine
// returns exactly one value, a string. It is a source check rather than a behavioural one because the
// property is an ABSENCE — there is no error to observe — and the only way an absence regresses is a
// signature change.
func assertHoldMachineCannotFailAnAttempt(t *testing.T) {
	t.Helper()
	const path = "../execution/machine_occupancy.go"
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "holdMachine" {
			continue
		}
		results := fn.Type.Results
		if results == nil || len(results.List) != 1 {
			t.Fatalf("holdMachine returns %d values, want exactly 1 — a second result is an error path, and a capacity refusal must not be able to end an attempt while occupancies still leak and nothing wakes on a freed slot",
				len(results.List))
		}
		if name, ok := results.List[0].Type.(*ast.Ident); !ok || name.Name != "string" {
			t.Fatalf("holdMachine returns a %v, want string — see above", results.List[0].Type)
		}
		return
	}
	t.Fatalf("no holdMachine declaration in %s — this guard is measuring nothing", path)
}
