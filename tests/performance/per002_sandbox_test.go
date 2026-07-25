//go:build performance

// PER-002 — cold/warm sandbox: assignment→engine-ready phase budgets (spec §64.14, §54.3).
//
// The chain measured here is the REAL one, phase by phase, with nothing simulated:
//
//	workspace_create   coordinator.CreateWorkspace   (E09 §29.7 logical workspace row)
//	workspace_allocate coordinator.AllocateWorkspace (physical allocation + fencing token)
//	lease_acquire      coordinator.AcquireWriterLease(E09 §29.8 single-writer lease)
//	engine_ready       runner.StreamSupervisor       (real OCI container, real §25.6 handshake,
//	                                                  timestamped when engine.ready reaches the sink)
//	assign_to_ready    the sum: what §54.3 calls assignment-to-engine-ready
//
// §54.3 budgets warm at p95 < 10s and cold at < 90s, and the two are recorded as separate metrics
// because the spec budgets them separately. But "COLD" HERE IS A POSITION, NOT A TEMPERATURE: it is the
// first attempt in this process, and nothing around it is cold. The tier script built the fixture image
// immediately before, Postgres is already migrated, and the OCI driver is constructed in requireSandbox
// before attempt 1 ever runs. The numbers refute the label outright — on this profile the "cold" attempt
// is routinely the FASTEST of the six (n=1 at ~505ms against a warm p95 of ~2.0s), because the warm
// attempts are the ones contending with the daemon this run just filled. WARM means every attempt after
// the first.
//
// So: §54.3's cold-start budget is NOT established here, and the cold metric's "p95" is a SINGLE
// observation. A real cold-host/cold-daemon measurement is E18 §6 operator leg 3.
//
// HONEST CEILING: macOS + Docker Desktop, NO SLO and NO reference-hardware claim. The fixture image is
// already local, so the number excludes image pull — which is what §54.3's own "excluding large user
// image pull" says, and it is stated here rather than implied. The engine is the deterministic fixture
// (E08 rule: no tools to a real provider), so this measures allocation + container start + handshake,
// never model work. Capacity behaviour under concurrent allocation pressure is NOT measured: attempts
// run one at a time, so no capacity or saturation claim is made.
package performance

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/storage"
)

const per002Ceiling = "macOS + Docker Desktop profile; NO SLO and NO reference-hardware claim. 'COLD' HERE " +
	"MEANS ONLY 'the first attempt in this process': the fixture image was just built LOCALLY (so the number " +
	"excludes image pull, §54.3's own carve-out), Postgres is already migrated, and the OCI driver is " +
	"constructed before attempt 1 — the daemon and the DB are WARM. The cold metric is therefore n=1 and its " +
	"'p95' is that single observation; on this profile it is typically the FASTEST attempt of the run, which " +
	"is why it must not be read as a cold start. §54.3's cold-start budget is NOT established here — E18 §6 " +
	"operator leg 3 measures it. The engine is the deterministic fixture per the E08 rule — allocation + " +
	"container start + §25.6 handshake, never model work. Attempts are sequential: no capacity or saturation " +
	"behaviour is measured or claimed."

// per002Attempts is deliberately small: this tier shares an 8GB Docker Desktop with other work, and the
// phase budget is a per-attempt figure, not a throughput one.
const per002Attempts = 6

func TestPER002ColdWarmSandboxPhases(t *testing.T) {
	sb := requireSandbox(t)
	run := mustRun(t, "PER-002", fmt.Sprintf(
		"%d sequential attempts (attempt 1 labelled 'cold' = first in this process, on an already-warm "+
			"daemon and DB; %d warm): real workspace row + allocation + writer lease against Postgres, then "+
			"a real digest-pinned fixture-engine container to engine.ready",
		per002Attempts, per002Attempts-1), per002Ceiling)

	// The cold gate's source says what it is AT THE POINT IT IS READ: §54.3's ceiling applied to one
	// warm-daemon observation is not a §54.3 cold-start result, and "pass" here means only that the first
	// attempt stayed under a very loose bound.
	run.Gate("assign_to_ready_cold", 95, float64(90*time.Second), "ns",
		"spec §54.3's cold CEILING (p95 < 90s, excl. large image pull) applied to n=1 — the first "+
			"in-process attempt on an already-warm daemon and DB, NOT a cold-host measurement and NOT a "+
			"§54.3 cold-start result (see the profile ceiling; E18 §6 leg 3 measures it)")
	run.Gate("assign_to_ready_warm", 95, float64(10*time.Second), "ns",
		"spec §54.3 (warm assignment-to-engine-ready p95 < 10s)")
	run.MaxErrorRate(0)

	for i := 0; i < per002Attempts; i++ {
		phase := "warm"
		if i == 0 {
			phase = "cold" // attempt 1's POSITION, not its temperature — see the package comment
		}
		sb.measureAssignment(t, run, phase, "fastexit", nil)
	}
	complete(t, run, "PER-002")
}

// TestPER002SlowEngineFixtureFailsThePhaseBudget is the RED fixture: the SAME chain with an engine that
// really is slow to answer the handshake (the slowready fixture mode sleeps before engine.ready). The
// container, the allocation and the lease are all real — only the engine is slow — and the phase budget
// must refuse the run.
func TestPER002SlowEngineFixtureFailsThePhaseBudget(t *testing.T) {
	sb := requireSandbox(t)
	const delay = 3 * time.Second
	run := mustRun(t, "PER-002-negative-slow-engine", fmt.Sprintf(
		"RED fixture: one attempt whose engine sleeps %s before engine.ready, against a %s warm budget",
		delay, per002SlowBudget), per002Ceiling+" RED fixture: produces a deliberately failing run.")
	// The budget is tightened to a value the slow fixture must breach. Tightening it is the point: the
	// gate is being tested, not the machine, and the tightened value is recorded in the evidence.
	run.Gate("assign_to_ready_warm", 95, float64(per002SlowBudget), "ns",
		"RED fixture budget (tightened below the deliberate engine delay to exercise the gate)")

	sb.measureAssignment(t, run, "warm", "slowready",
		map[string]string{"PALAI_FIXTURE_READY_DELAY_MS": fmt.Sprint(delay.Milliseconds())})
	mustFailGate(t, run, "PER-002-negative-slow-engine", "assign_to_ready_warm")
}

// per002SlowBudget sits between a healthy warm attempt (well under a second on this profile) and the
// RED fixture's 3s delay, so the failure is caused by the slow engine and not by a budget no attempt
// could ever meet.
const per002SlowBudget = 1500 * time.Millisecond

// sandbox is the PER-002 environment: the durable spine for the E09 rows plus the immutable fixture
// image id the tier script exported.
type sandbox struct {
	spine  *coordinator.Store
	tenant coordinator.Tenant
	digest string
	driver oci.InteractiveDriver
}

func requireSandbox(t *testing.T) *sandbox {
	t.Helper()
	pgURL := os.Getenv("PALAI_PERF_POSTGRES_URL")
	digest := os.Getenv("PALAI_RUNNER_ENGINE_IMAGE_ID")
	if pgURL == "" || digest == "" {
		t.Skip("PALAI_PERF_POSTGRES_URL + PALAI_RUNNER_ENGINE_IMAGE_ID are required; run make test-performance TEST=sandbox")
	}
	ctx := context.Background()
	spine, err := coordinator.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		t.Fatalf("create Docker interactive driver: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := driver.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				t.Errorf("close interactive driver: %v", err)
			}
		}
	})

	sb := &sandbox{spine: spine, digest: digest, driver: driver,
		tenant: coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}}
	sb.exec(t, `INSERT INTO organizations (id) VALUES ($1)`, sb.tenant.Organization)
	sb.exec(t, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, sb.tenant.Project, sb.tenant.Organization)
	return sb
}

func (sb *sandbox) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := sb.spine.Pool().Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

// measureAssignment times one full assignment→engine-ready chain and records each phase plus the total.
// A failed attempt is recorded as an error sample rather than dropped: an assignment that never reached
// engine.ready is the worst possible latency, not a missing data point.
func (sb *sandbox) measureAssignment(t *testing.T, run *Run, phase, mode string, extraEnv map[string]string) {
	t.Helper()
	ctx := storage.WithTenant(context.Background(), sb.tenant.Organization, sb.tenant.Project)

	sessionID, runID := newID("ses"), newID("run")
	sb.exec(t, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`,
		sessionID, sb.tenant.Organization, sb.tenant.Project)
	sb.exec(t, `INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'running')`,
		runID, sb.tenant.Organization, sb.tenant.Project, sessionID)

	hostPath := t.TempDir()
	total := time.Now()

	start := time.Now()
	workspaceID := newID("ws")
	if err := sb.spine.CreateWorkspace(ctx, sb.tenant, coordinator.WorkspaceInput{
		WorkspaceID: workspaceID, SessionID: sessionID, RunID: runID, State: "ready",
	}); err != nil {
		t.Fatalf("CreateWorkspace error = %v", err)
	}
	run.Latency("workspace_create", phase, time.Since(start), true)

	start = time.Now()
	alloc, err := sb.spine.AllocateWorkspace(ctx, newID("alloc"), workspaceID, hostPath)
	if err != nil {
		t.Fatalf("AllocateWorkspace error = %v", err)
	}
	run.Latency("workspace_allocate", phase, time.Since(start), true)

	start = time.Now()
	if err := sb.spine.AcquireWriterLease(ctx, newID("wls"), alloc.ID, runID); err != nil {
		t.Fatalf("AcquireWriterLease error = %v", err)
	}
	run.Latency("lease_acquire", phase, time.Since(start), true)

	env := map[string]string{"PALAI_ENGINE_MODE": mode}
	for k, v := range extraEnv {
		env[k] = v
	}
	request := runner.EngineRequest{
		ImageDigest:       sb.digest,
		RunID:             contracts.RunID(runID),
		AttemptID:         contracts.AttemptID(newID("att")),
		Fence:             uint64(alloc.Fence),
		Env:               env,
		WorkspaceHostPath: alloc.HostPath,
		Limits: runner.Limits{
			WallTimeMS: 60000, MaxStdoutBytes: 32 * 1024, MaxStderrBytes: 4 * 1024,
			MaxFrameBytes: 8 * 1024, MaxMemoryBytes: 64 * 1024 * 1024, MaxProcessCount: 16,
		},
	}

	// The sink timestamps engine.ready the moment the supervisor admits it — the actual event §54.3
	// names, not the container's exit.
	var ready time.Time
	start = time.Now()
	supervisor := runner.NewStreamSupervisor(sb.driver)
	_, streamErr := supervisor.Stream(context.Background(), request, nil,
		func(_ context.Context, frame contracts.EngineFrame) error {
			if frame.Type == "engine.ready" && ready.IsZero() {
				ready = time.Now()
			}
			return nil
		})
	if streamErr != nil {
		// Record the failure at the wall-clock cost it incurred, then fail: a lost attempt must not
		// disappear from the percentiles.
		run.Latency("engine_ready", phase, time.Since(start), false)
		run.Latency("assign_to_ready_"+phase, phase, time.Since(total), false)
		t.Fatalf("Stream(%s) error = %v", mode, streamErr)
	}
	if ready.IsZero() {
		t.Fatal("the attempt succeeded but no engine.ready frame reached the sink")
	}
	run.Latency("engine_ready", phase, ready.Sub(start), true)
	run.Latency("assign_to_ready_"+phase, phase, ready.Sub(total), true)
}
