//go:build component

package fleet_test

// WHAT A MACHINE THAT DECLARED NOTHING HOLDS (Faz A.4 T5).
//
// The ceiling itself is proved in tests/component/fleet, over machines whose capacity the fixture writes.
// This one is here for the reason store_component_test.go is here: it drives ENROLMENT, and enrolment is
// `fleet.Store.Register`, which Go forbids importing from outside apps/control-plane. A test that inserted
// its own runner row could assert what a stored 1 does; only this one can assert where the 1 comes from.
//
// AND THAT IS THE WHOLE POINT, because measured 2026-08-05 NO MACHINE DECLARES A CAPACITY AT ALL:
//
//	grep -rn 'Registration{' --include='*.go' . | grep -v node_modules
//	→ runner_gateway.go:754 (the one production site) + this package's own test helpers
//
// and that site sets ID, PoolID, Label, DNS, PublicKeySHA256, OS, Arch, Posture and KeyID — not Capacity.
// `packages/runner` never says the word, so the enrolment request carries no such field, and
// storage/queries/runners.sql has no UPDATE that could change one later. So `reg.Capacity` is 0 on every
// enrolment there has ever been, store.go's clamp turns it into 1, and 1 is what every machine in every
// deployment carries.
//
// THE CEILING THEREFORE HAS TO BE EXACTLY ONE, AND THAT IS AN ASSERTION AND NOT A DESCRIPTION. The two
// wrong answers are opposite and both are reachable from a plausible implementation: a ceiling that read a
// zero would be INFINITE (nothing < 0 is false for a count, so `count(*) < 0` refuses everything — while a
// build that treated 0 as "unlimited" refuses nothing), and a ceiling that hard-coded 1 would ignore the
// column. The first is caught here; the second is caught by the three-capacity machine in
// tests/component/fleet, and neither test catches the other's.

import (
	"context"
	"errors"
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

// TestAMachineThatDeclaredNoCapacityHoldsExactlyOne — ONE, NOT INFINITY, AND NOT A CONSTANT.
//
// A machine enrols the way every machine in this tree enrols: saying nothing about how much it can hold.
// The registry stores 1 and the placement ceiling holds it to 1 — the second session is refused, and it is
// refused because the MACHINE is full rather than because anything is wrong with that session, which the
// last two blocks separate.
func TestAMachineThatDeclaredNoCapacityHoldsExactlyOne(t *testing.T) {
	pool := openPool(t)
	spine := openSpine(t) // migrates, so the fixture below has tables to write into
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	ctx := context.Background()
	tenant := coordinator.Tenant{Project: project}

	declaring := enrollment(poolID, "runner-local")
	// NOT SET, and that absence is the subject: the production enrolment site sets no Capacity either.
	if declaring.Capacity != 0 {
		t.Fatalf("the fixture declared capacity %d — this test is about a machine that declares NOTHING", declaring.Capacity)
	}
	machine, err := registry.Register(ctx, declaring)
	if err != nil {
		t.Fatalf("register a machine that declares no capacity: %v", err)
	}
	if machine.Capacity != 1 {
		t.Fatalf("a machine that declared nothing was stored with capacity %d, want 1 — store.go's clamp is what turns an undeclared capacity into a real one, and every machine in this tree takes that path", machine.Capacity)
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

	held, second := seedSession(), seedSession()
	if _, err := spine.AcquireLease(ctx, tenant, held, machine.ID); err != nil {
		t.Fatalf("the FIRST hold on a freshly enrolled machine = %v, want success — a machine that can hold nothing at all would make the refusal below meaningless", err)
	}
	if _, err := spine.AcquireLease(ctx, tenant, second, machine.ID); !errors.Is(err, coordinator.ErrMachineAtCapacity) {
		t.Fatalf("the SECOND hold on a machine that declared nothing = %v, want ErrMachineAtCapacity — an undeclared capacity is being read as unlimited, so one Mac would take every session on the stack", err)
	}

	// THE REFUSAL IS THE MACHINE'S AND NOT THE SESSION'S. A second machine enrolled exactly the same way
	// takes the same session immediately, so nothing above was a property of that session or of this
	// tenant having already held something.
	spare, err := registry.Register(ctx, enrollment(poolID, "runner-local"))
	if err != nil {
		t.Fatalf("register a second machine: %v", err)
	}
	if _, err := spine.AcquireLease(ctx, tenant, second, spare.ID); err != nil {
		t.Fatalf("the refused session on a second machine = %v, want success — the ceiling is refusing the session rather than counting the machine's occupancies", err)
	}
}
