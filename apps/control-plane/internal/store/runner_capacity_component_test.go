//go:build component

package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/storage"
)

// THE SERVER'S OCCUPANCY CEILING FOLLOWS THE CONCURRENCY THE MACHINE SAYS IT APPLIED, against real
// PostgreSQL.
//
// ‼️ `runners.capacity` HAD NO WRITER AFTER ENROLMENT, WHILE RecoverRunner's comment CALLS IT "the ADMIN
// plane's number since plan §3.6". Nothing made that true. It was written once, at Register, from the
// device's `PALAI_RUNNER_CAPACITY` — a variable the device path DELETED (§3.7) — so a packaged agent
// enrolled with `capacity = 0`, which AcquireLease reads as NO CEILING, while the admin plane's document
// had it serving N lease loops. Placement then handed that machine more occupancies than it had loops to
// run them on, and the extra sessions waited on a loop that never frees. Nothing was red: both halves
// behaved exactly as designed, and no test drove them together.

// capacityEnv is one pool with one machine, and the desired document that machine polls.
type capacityEnv struct {
	repo    *Store
	project string
	pool    string
	runner  string
	dns     string
}

func newCapacityEnv(t *testing.T) *capacityEnv {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	e := &capacityEnv{repo: repo, project: capacityID("prj"), pool: capacityID("pool"), runner: capacityID("rnr")}
	e.dns = e.runner + ".runner.palai.internal"
	e.exec(t, `INSERT INTO projects (id) VALUES ($1)`, e.project)
	e.exec(t, `INSERT INTO runner_pools (id, project_id, name, posture)
	           VALUES ($1, $2, $3, 'unsandboxed-host')`, e.pool, e.project, capacityID("name"))
	// capacity 0 is the SHIPPED state of a packaged device: it reads no PALAI_ variables, so it declares
	// nothing, and 0 means "no ceiling" to AcquireLease.
	e.exec(t, `INSERT INTO runners (id, project_id, pool_id, state, posture, runner_dns, capacity)
	           VALUES ($1, $2, $3, 'active', 'unsandboxed-host', $4, 0)`, e.runner, e.project, e.pool, e.dns)
	return e
}

// capacityID mints a collision-free fixture id. This package's component tier shares ONE database, so a
// fixed id would make two runs of this file the same machine.
func capacityID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func (e *capacityEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.repo.Spine().Pool().Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *capacityEnv) capacity(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.repo.Spine().Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT capacity FROM runners WHERE id=$1`, e.runner).Scan(&n); err != nil {
		t.Fatalf("read capacity: %v", err)
	}
	return n
}

// desire writes the pool's desired document THROUGH THE PRODUCTION WRITER and returns the revision this
// machine would be served. Hand-rolled SQL here would seed a shape PutDesiredConfig never produces —
// including the revision, which is a generated identity shared across planes.
func (e *capacityEnv) desire(t *testing.T, concurrency string) int64 {
	t.Helper()
	body := `{"plane":"` + api.PlaneRunnerPool + `","scope_id":"` + e.pool +
		`","settings":{"PALAI_RUNNER_CONCURRENCY":"` + concurrency + `"}}`
	if _, err := e.repo.PutDesiredConfig(context.Background(), operatorScope(), []byte(body)); err != nil {
		t.Fatalf("write desired document: %v", err)
	}
	_, revision, err := e.repo.DesiredSettingsForMachine(context.Background(), e.pool, e.runner)
	if err != nil {
		t.Fatalf("resolve desired settings: %v", err)
	}
	return revision
}

// operatorScope is the verified caller PutDesiredConfig records as the author.
func operatorScope() middleware.Scope {
	return middleware.Scope{Project: "system", Principal: "capacity-component-test", APIKeyID: "key_capacity_component"}
}

func (e *capacityEnv) report(t *testing.T, revision int64, verdict string) {
	t.Helper()
	matched, err := e.repo.RecordRunnerConfigReport(context.Background(), e.dns, revision,
		map[string]string{"PALAI_RUNNER_CONCURRENCY": verdict}, time.Now().UTC())
	if err != nil {
		t.Fatalf("record report: %v", err)
	}
	if !matched {
		t.Fatalf("the report matched no registry row for %s", e.dns)
	}
}

// TestTheCeilingFollowsTheConcurrencyTheMachineSaysItApplied is the property in one line: after the
// machine reports it applied the plane's number, the server refuses the occupancy that number forbids.
func TestTheCeilingFollowsTheConcurrencyTheMachineSaysItApplied(t *testing.T) {
	e := newCapacityEnv(t)
	if got := e.capacity(t); got != 0 {
		t.Fatalf("the fixture starts at capacity %d, want 0 — a packaged device declares nothing", got)
	}

	revision := e.desire(t, "4")
	e.report(t, revision, runner.VerdictApplied)
	if got := e.capacity(t); got != 4 {
		t.Fatalf("capacity = %d after the machine reported it applied 4 — the placement would keep handing "+
			"this machine occupancies it has no lease loop for", got)
	}

	// DOWN as well as up. A pool lowered from 4 to 1 leaves a machine serving one loop; a ceiling stuck at
	// 4 admits three sessions that then wait on loops that no longer exist.
	revision = e.desire(t, "1")
	e.report(t, revision, runner.VerdictApplied)
	if got := e.capacity(t); got != 1 {
		t.Fatalf("capacity = %d after the machine reported it applied 1, want 1", got)
	}
}

// TestAMachineThatDidNotApplyTheValueKeepsItsCeiling is the other half, and it is the reason the verdict
// is consulted at all: a control plane that set the ceiling when an OPERATOR SAVED the value would ceiling
// a machine at a number it never took.
func TestAMachineThatDidNotApplyTheValueKeepsItsCeiling(t *testing.T) {
	e := newCapacityEnv(t)
	revision := e.desire(t, "4")
	e.report(t, revision, runner.VerdictApplied)
	if got := e.capacity(t); got != 4 {
		t.Fatalf("capacity = %d, want 4 — the baseline this test perturbs from is not in place", got)
	}

	for _, verdict := range []string{runner.VerdictNotRead, runner.RefusedVerdict("not a positive integer")} {
		next := e.desire(t, "9")
		e.report(t, next, verdict)
		if got := e.capacity(t); got != 4 {
			t.Fatalf("a machine that reported %q for concurrency 9 was ceilinged at %d — the server would "+
				"place sessions on loops the machine told us it is not running", verdict, got)
		}
	}
}

// TestAReportFromAnOlderRevisionDoesNotWriteTodaysNumber guards the stale-measurement trap. A machine
// still on revision N applied revision N's value; resolving the CURRENT document for it would write a
// ceiling from a document it has never seen.
func TestAReportFromAnOlderRevisionDoesNotWriteTodaysNumber(t *testing.T) {
	e := newCapacityEnv(t)
	old := e.desire(t, "2")
	e.report(t, old, runner.VerdictApplied)
	if got := e.capacity(t); got != 2 {
		t.Fatalf("capacity = %d, want 2", got)
	}

	e.desire(t, "8") // the plane moves on; the machine has not polled it yet
	e.report(t, old, runner.VerdictApplied)
	if got := e.capacity(t); got != 2 {
		t.Fatalf("a report for revision %d wrote the CURRENT document's number (capacity = %d, want 2) — the "+
			"ceiling now comes from a document this machine has never seen", old, got)
	}
}
