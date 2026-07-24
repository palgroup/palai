package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// Store is the Postgres-backed CapabilityWorker persistence (migration 000039). Every read/write runs under a
// scoped context so RLS (000029/000039) confines the rows to the worker's tenant; the tenant is always the
// enrolling worker's verified scope, never anything the worker sends. It holds a SecretResolver to redeem
// job-scoped handles — the resolved value is returned to the caller and never written back here.
type Store struct {
	pool    *pgxpool.Pool
	secrets SecretResolver
	newID   func(prefix string) string
	now     func() time.Time
}

// NewStore builds the store. newID mints row/job ids (pass middleware.NewID in production); now defaults to
// time.Now and is overridable for deterministic deadline/expiry tests. secrets may be nil when no job uses a
// secret handle, but RedeemSecretHandle then errors.
func NewStore(pool *pgxpool.Pool, secrets SecretResolver, newID func(prefix string) string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{pool: pool, secrets: secrets, newID: newID, now: now}
}

func (s *Store) mintID(prefix string) string {
	if s.newID != nil {
		return s.newID(prefix)
	}
	return prefix + "_" + fmt.Sprint(s.now().UnixNano())
}

// Enroll registers a worker for a typed capability. The capability MUST be one the control plane types
// (Catalog) — enrollment for an unknown/disabled capability (e.g. apple-build) is refused, so the surface can
// only ever run operations that exist. The returned Worker carries fence 1; a later health/capability change
// bumps it, cutting any lease held under the old fence.
func (s *Store) Enroll(ctx context.Context, tenant Tenant, spec WorkerSpec) (Worker, error) {
	if tenant.Organization == "" || tenant.Project == "" {
		return Worker{}, errors.New("workers: enrollment requires an org and project scope")
	}
	if !KnownCapability(spec.Capability) {
		return Worker{}, ErrUnknownCapability
	}
	if spec.Capacity <= 0 {
		spec.Capacity = 1
	}
	if spec.TrustLabel == "" {
		spec.TrustLabel = "sandbox"
	}
	id := s.mintID("cwk")
	if spec.ToolchainDigests == nil {
		spec.ToolchainDigests = map[string]string{}
	}
	digests, err := json.Marshal(spec.ToolchainDigests)
	if err != nil {
		return Worker{}, fmt.Errorf("marshal toolchain digests: %w", err)
	}
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	_, err = s.pool.Exec(ctx, storage.Query("InsertCapabilityWorker"),
		id, tenant.Organization, tenant.Project, spec.Capability, orDefault(spec.CapabilityVersion, "0.1.0"),
		spec.OS, spec.Arch, digests, spec.Capacity, spec.PoolLabel, spec.TrustLabel)
	if err != nil {
		return Worker{}, fmt.Errorf("insert capability worker: %w", err)
	}
	return Worker{ID: id, Tenant: tenant, Spec: spec, Health: "healthy", Fence: 1}, nil
}

// GetWorker resolves an enrolled worker within its tenant.
func (s *Store) GetWorker(ctx context.Context, tenant Tenant, workerID string) (Worker, bool, error) {
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	var w Worker
	var digests []byte
	w.Tenant = tenant
	err := s.pool.QueryRow(ctx, storage.Query("GetCapabilityWorker"), workerID, tenant.Organization, tenant.Project).
		Scan(&w.ID, &w.Tenant.Organization, &w.Tenant.Project, &w.Spec.Capability, &w.Spec.CapabilityVersion,
			&w.Spec.OS, &w.Spec.Arch, &digests, &w.Spec.Capacity, &w.Spec.PoolLabel, &w.Spec.TrustLabel, &w.Health, &w.Fence)
	if errors.Is(err, pgx.ErrNoRows) {
		return Worker{}, false, nil
	}
	if err != nil {
		return Worker{}, false, fmt.Errorf("get capability worker: %w", err)
	}
	if len(digests) > 0 {
		_ = json.Unmarshal(digests, &w.Spec.ToolchainDigests)
	}
	return w, true, nil
}

// SetWorkerHealth records a health change and BUMPS the worker's enrollment fence, cutting any lease it holds
// (§31.6: a health/capability change cuts the new lease). Returns the new fence.
func (s *Store) SetWorkerHealth(ctx context.Context, tenant Tenant, workerID, health string) (int64, error) {
	if health != "healthy" && health != "draining" && health != "unhealthy" {
		return 0, fmt.Errorf("workers: invalid health %q", health)
	}
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	var fence int64
	err := s.pool.QueryRow(ctx, storage.Query("SetCapabilityWorkerHealth"), workerID, tenant.Organization, tenant.Project, health).Scan(&fence)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("workers: no such worker")
	}
	if err != nil {
		return 0, fmt.Errorf("set worker health: %w", err)
	}
	return fence, nil
}

// DispatchJob appends the immutable dispatch entry for a job (§31.3). It refuses an operation that is not a
// typed operation of its capability (ErrUntypedOperation — the no-tunnel gate at dispatch). Idempotent: a
// second dispatch under the same idempotency key returns the existing job_id rather than a second job.
func (s *Store) DispatchJob(ctx context.Context, tenant Tenant, spec JobSpec) (string, error) {
	if _, ok := LookupOperation(spec.Capability, spec.Operation); !ok {
		return "", ErrUntypedOperation
	}
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)

	if spec.IdempotencyKey != "" {
		var existing string
		err := s.pool.QueryRow(ctx, storage.Query("JobByIdempotencyKey"), tenant.Organization, tenant.Project, spec.IdempotencyKey).Scan(&existing)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("lookup idempotency key: %w", err)
		}
	}

	jobID := spec.JobID
	if jobID == "" {
		jobID = s.mintID("cjob")
	} else {
		// MINOR 4: never re-open an existing job with a fresh 'dispatched' at fence 1 — that wedges it (latest
		// fence 1 != MAX ⇒ every submit is ErrStaleFence). A re-dispatch is RedispatchForRetry (fence+1).
		var exists bool
		if err := s.pool.QueryRow(ctx, storage.Query("JobHasEntries"), jobID, tenant.Organization, tenant.Project).Scan(&exists); err != nil {
			return "", fmt.Errorf("check job exists: %w", err)
		}
		if exists {
			return "", ErrJobExists
		}
	}
	if err := s.appendEntry(ctx, tenant, entry{
		jobID: jobID, kind: "dispatched", idempotencyKey: spec.IdempotencyKey, runID: spec.RunID,
		attemptID: spec.AttemptID, workerID: "", capability: spec.Capability, operation: spec.Operation,
		inputRefs: spec.InputRefs, secretHandleRefs: spec.SecretHandleRefs, deadline: spec.Deadline,
		resourceLimits: spec.ResourceLimits, outputSchema: spec.OutputSchema, networkPolicy: spec.NetworkPolicy,
		sideEffectKey: spec.SideEffectKey, fence: 1, receipt: nil,
	}); err != nil {
		// A racing duplicate dispatch trips the idempotency partial-unique index; resolve to the winner.
		if isUniqueViolation(err) && spec.IdempotencyKey != "" {
			var existing string
			if e := s.pool.QueryRow(ctx, storage.Query("JobByIdempotencyKey"), tenant.Organization, tenant.Project, spec.IdempotencyKey).Scan(&existing); e == nil {
				return existing, nil
			}
		}
		return "", err
	}
	return jobID, nil
}

// ClaimNext leases ONE ready job for the worker's capability and appends a 'leased' entry at the dispatch
// fence. It re-reads the worker to confirm it is healthy (an unhealthy/draining worker claims nothing). The
// returned Claim carries both fences the §31.6 guard checks. A false second return means no ready job.
func (s *Store) ClaimNext(ctx context.Context, tenant Tenant, workerID string) (Claim, bool, error) {
	worker, ok, err := s.GetWorker(ctx, tenant, workerID)
	if err != nil {
		return Claim{}, false, err
	}
	if !ok || worker.Health != "healthy" {
		return Claim{}, false, nil
	}
	sctx := storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	var jobID, operation, sideEffectKey, runID, attemptID string
	var deadline *time.Time
	var jobFence int64
	var inputRefs, secretRefs, resourceLimits, outputSchema, networkPolicy []byte
	err = s.pool.QueryRow(sctx, storage.Query("ReadyCapabilityJob"), tenant.Organization, tenant.Project, worker.Spec.Capability).
		Scan(&jobID, &operation, &deadline, &jobFence, &inputRefs, &secretRefs, &resourceLimits, &outputSchema, &networkPolicy, &sideEffectKey, &runID, &attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, false, nil
	}
	if err != nil {
		return Claim{}, false, fmt.Errorf("find ready job: %w", err)
	}

	op, typed := LookupOperation(worker.Spec.Capability, operation)
	if !typed {
		// Defence in depth: a job whose operation is not typed for this capability is never handed to a
		// worker (it could not have been dispatched, but the claim path refuses it anyway — no tunnel).
		return Claim{}, false, ErrUntypedOperation
	}

	fireAfterFenceGuard()
	if err := s.appendEntry(sctx, tenant, entry{
		jobID: jobID, kind: "leased", workerID: workerID, capability: worker.Spec.Capability,
		operation: operation, fence: jobFence, sideEffectKey: sideEffectKey,
		guarded: true, workerFence: worker.Fence,
	}); err != nil {
		// A stale lease — the job fence was bumped by a re-dispatch (or the worker drained) between the ready
		// read and this append — or a lost seq race both mean the lease did not land. The job stays claimable
		// by the next worker; a stale 'leased' entry never buries the re-dispatch.
		if errors.Is(err, errFenceGuardMiss) || isUniqueViolation(err) {
			return Claim{}, false, nil
		}
		return Claim{}, false, err
	}

	claim := Claim{
		JobID: jobID, Tenant: tenant, WorkerID: workerID, JobFence: jobFence, WorkerFence: worker.Fence,
		Capability: worker.Spec.Capability, Operation: operation, ReadOnly: op.ReadOnly,
		InputRefs: decodeStrings(inputRefs), SecretHandleRefs: decodeStrings(secretRefs),
		OutputSchema: decodeMap(outputSchema), SideEffectKey: sideEffectKey,
	}
	if deadline != nil {
		claim.Deadline = *deadline
	}
	return claim, true, nil
}

// SubmitResult records a worker's terminal outcome after the §31.6 fence guard passes. An "uncertain" class
// quarantines the job (it is NOT retried); "completed"/"failed" are recorded as such. The submitted operation
// must equal the claim's typed operation (a re-typed submit is the no-tunnel refusal). The receipt is written
// verbatim — the CONTRACT is that the worker never places a secret value in it; the store never adds one.
func (s *Store) SubmitResult(ctx context.Context, claim Claim, outcome Outcome) error {
	if outcome.Operation != claim.Operation {
		return ErrUntypedOperation
	}
	if _, ok := LookupOperation(claim.Capability, outcome.Operation); !ok {
		return ErrUntypedOperation
	}
	if err := s.guardFences(ctx, claim); err != nil {
		return err
	}
	kind := "completed"
	switch outcome.Class {
	case "completed":
		kind = "completed"
	case "failed":
		kind = "failed"
	case "uncertain":
		// §31.6: an uncertain failure quarantines the job rather than retrying it — a side effect may or may
		// not have happened, so a blind retry is unsafe.
		kind = "quarantined"
	default:
		return fmt.Errorf("workers: invalid outcome class %q", outcome.Class)
	}
	fireAfterFenceGuard()
	sctx := storage.WithTenant(ctx, claim.Tenant.Organization, claim.Tenant.Project)
	if err := s.appendEntry(sctx, claim.Tenant, entry{
		jobID: claim.JobID, kind: kind, workerID: claim.WorkerID, capability: claim.Capability,
		operation: claim.Operation, fence: claim.JobFence, receipt: orEmptyMap(outcome.Receipt),
		outputRefs: outcome.OutputRefs, sideEffectKey: claim.SideEffectKey,
		guarded: true, workerFence: claim.WorkerFence,
	}); err != nil {
		if errors.Is(err, errFenceGuardMiss) {
			// The guard passed but the world moved before the terminal append landed: re-read to name the
			// precise reason (stale job fence / worker fenced / lease lost), else fall back to stale fence.
			if ge := s.guardFences(ctx, claim); ge != nil {
				return ge
			}
			return ErrStaleFence
		}
		return err
	}
	return nil
}

// RedispatchForRetry re-dispatches a job to another compatible worker by appending a NEW dispatched entry at
// fence+1, which fences out the old worker (§31.6). It refuses a side-effecting operation (only a read-only
// op retries blindly; a side-effecting one relies on destination idempotency). Returns the new fence.
func (s *Store) RedispatchForRetry(ctx context.Context, tenant Tenant, jobID string) (int64, error) {
	sctx := storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	cur, err := s.currentJob(sctx, tenant, jobID)
	if err != nil {
		return 0, err
	}
	op, typed := LookupOperation(cur.capability, cur.operation)
	if !typed {
		return 0, ErrUntypedOperation
	}
	if !op.ReadOnly {
		return 0, ErrNotRetryable
	}
	newFence := cur.fence + 1
	var deadline time.Time
	if cur.deadline != nil {
		deadline = *cur.deadline
	}
	if err := s.appendEntry(sctx, tenant, entry{
		jobID: jobID, kind: "dispatched", idempotencyKey: cur.idempotencyKey, runID: cur.runID,
		attemptID: cur.attemptID, capability: cur.capability, operation: cur.operation,
		inputRefs: cur.inputRefs, secretHandleRefs: cur.secretHandleRefs, deadline: deadline,
		sideEffectKey: cur.sideEffectKey, fence: newFence,
	}); err != nil {
		return 0, err
	}
	return newFence, nil
}

// RedeemSecretHandle resolves a job-scoped, short-lived secret handle to its VALUE (§31.5/§35 secret handles).
// It is the crown security path, so it enforces every guard before it resolves anything:
//   - the fence guard (a fenced-out worker cannot redeem);
//   - scope: the handle name MUST be one the DISPATCH entry named for THIS job (ErrHandleNotScoped) — read
//     authoritatively from the journal, never trusted from the claim;
//   - expiry: now must be at or before the job deadline (ErrHandleExpired) — the handle expires WITH the
//     deadline.
//
// The returned value is handed to the worker for use and is NEVER written to the journal, a log, or evidence.
func (s *Store) RedeemSecretHandle(ctx context.Context, claim Claim, handleName string) ([]byte, error) {
	if s.secrets == nil {
		return nil, errors.New("workers: no secret resolver configured")
	}
	if err := s.guardFences(ctx, claim); err != nil {
		return nil, err
	}
	sctx := storage.WithTenant(ctx, claim.Tenant.Organization, claim.Tenant.Project)
	cur, err := s.currentJob(sctx, claim.Tenant, claim.JobID)
	if err != nil {
		return nil, err
	}
	if !containsString(cur.secretHandleRefs, handleName) {
		return nil, ErrHandleNotScoped
	}
	// The handle is redeemable strictly BEFORE the deadline; at or past it, it has expired with the job.
	if cur.deadline == nil || !s.now().Before(*cur.deadline) {
		return nil, ErrHandleExpired
	}
	value, ok, err := s.secrets.Resolve(sctx, claim.Tenant.Organization, handleName)
	if err != nil {
		return nil, fmt.Errorf("resolve secret handle: %w", err)
	}
	if !ok {
		return nil, errors.New("workers: secret handle names no secret")
	}
	return value, nil
}

// --- internal helpers -------------------------------------------------------

// currentState is the §31.6 fence-guard snapshot of a job.
type currentState struct {
	fence            int64
	kind             string
	workerID         string
	deadline         *time.Time
	secretHandleRefs []string
	// spec fields, read from the dispatch entry for a re-dispatch.
	idempotencyKey string
	runID          string
	attemptID      string
	capability     string
	operation      string
	inputRefs      []string
	sideEffectKey  string
}

// guardFences rejects a claim whose JOB fence is no longer current (a re-dispatch bumped it — ErrStaleFence)
// or whose WORKER enrollment fence is no longer current (a health/capability change bumped it —
// ErrWorkerFenced). Both are the §31.6 lease cuts.
func (s *Store) guardFences(ctx context.Context, claim Claim) error {
	sctx := storage.WithTenant(ctx, claim.Tenant.Organization, claim.Tenant.Project)
	cur, err := s.currentJob(sctx, claim.Tenant, claim.JobID)
	if err != nil {
		return err
	}
	if cur.fence != claim.JobFence {
		return ErrStaleFence
	}
	worker, ok, err := s.GetWorker(sctx, claim.Tenant, claim.WorkerID)
	if err != nil {
		return err
	}
	if !ok || worker.Fence != claim.WorkerFence {
		return ErrWorkerFenced
	}
	// SHOULD-FIX 2: the job's CURRENT lease must be THIS worker's — the latest entry is 'leased' AND its
	// worker_id is the claimant. A caller forging a claim for a job it never leased (fences are small,
	// guessable ints) clears the fence checks but fails here; a double-terminal submit (latest already
	// terminal, not 'leased') is refused too. Leaseholder identity can change only via a re-dispatch, which
	// bumps the fence — so the guarded append's atomic fence re-check backstops this read.
	if cur.kind != "leased" || cur.workerID != claim.WorkerID {
		return ErrNotLeaseholder
	}
	return nil
}

// currentJob reads a job's §31.6 snapshot: MAX(fence_token) (monotonic, tamper-evident on the append-only
// journal), the latest-seq entry's kind/worker, the dispatch deadline + secret refs, and the dispatch spec
// (for a re-dispatch). A missing job is ErrNoSuchJob.
func (s *Store) currentJob(ctx context.Context, tenant Tenant, jobID string) (currentState, error) {
	sctx := storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	var cur currentState
	var refs []byte
	err := s.pool.QueryRow(sctx, storage.Query("CurrentCapabilityJob"), jobID, tenant.Organization, tenant.Project).
		Scan(&cur.fence, &cur.kind, &cur.workerID, &cur.deadline, &refs)
	if errors.Is(err, pgx.ErrNoRows) {
		return currentState{}, ErrNoSuchJob
	}
	if err != nil {
		return currentState{}, fmt.Errorf("read current job: %w", err)
	}
	cur.secretHandleRefs = decodeStrings(refs)
	// The dispatch spec (entry_seq = 1) — needed for a re-dispatch. Read it lazily via the ready-job shape.
	var operation, sideEffectKey, runID, attemptID, idem string
	var deadline *time.Time
	var jobFence int64
	var inputRefs, secretRefs []byte
	// ReadyCapabilityJob only returns dispatched-latest jobs; for a re-dispatch we need the dispatch spec
	// regardless of current kind, so read entry_seq = 1 directly.
	err = s.pool.QueryRow(sctx, storage.Query("JobDispatchSpec"), jobID, tenant.Organization, tenant.Project).
		Scan(&idem, &runID, &attemptID, &cur.capability, &operation, &inputRefs, &secretRefs, &deadline, &sideEffectKey, &jobFence)
	if err == nil {
		cur.idempotencyKey, cur.runID, cur.attemptID = idem, runID, attemptID
		cur.operation, cur.sideEffectKey = operation, sideEffectKey
		cur.inputRefs = decodeStrings(inputRefs)
		if deadline != nil {
			cur.deadline = deadline
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return currentState{}, fmt.Errorf("read job dispatch spec: %w", err)
	}
	return cur, nil
}

// entry is one immutable journal row to append.
type entry struct {
	jobID            string
	kind             string
	idempotencyKey   string
	runID            string
	attemptID        string
	workerID         string
	capability       string
	operation        string
	inputRefs        []string
	secretHandleRefs []string
	deadline         time.Time
	resourceLimits   map[string]any
	outputSchema     map[string]any
	networkPolicy    map[string]any
	sideEffectKey    string
	fence            int64
	receipt          map[string]any
	outputRefs       []string
	guarded          bool  // append via AppendGuardedJobEntry — the fence-guarded leased/terminal legs
	workerFence      int64 // the claim's worker enrollment fence, re-verified inside the guarded append
}

// errFenceGuardMiss is the guarded append's "did not land": the in-statement fence / worker-fence predicate
// no longer matched (a concurrent re-dispatch or health change advanced the world in the guard-then-append
// window), or the computed entry_seq lost the UNIQUE(job_id, entry_seq) race. Callers re-classify it —
// SubmitResult to the precise fence error, ClaimNext to a no-op no-claim.
var errFenceGuardMiss = errors.New("workers: guarded append did not land (fence no longer current)")

// afterFenceGuardHook, when non-nil, runs in the WINDOW between the fence guard read and the guarded journal
// append (SubmitResult and ClaimNext). It exists ONLY so a component test can DETERMINISTICALLY interleave a
// concurrent re-dispatch / health change into the exact TOCTOU window the guarded append must close; it is
// nil in every non-test build. ponytail: a package var over a Store field — the seam is test-only and the
// component tier runs -p 1, so a single set/reset per test is safe.
var afterFenceGuardHook func()

func fireAfterFenceGuard() {
	if afterFenceGuardHook != nil {
		afterFenceGuardHook()
	}
}

func (s *Store) appendEntry(ctx context.Context, tenant Tenant, e entry) error {
	sctx := storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	inputRefs, _ := json.Marshal(orEmptySlice(e.inputRefs))
	secretRefs, _ := json.Marshal(orEmptySlice(e.secretHandleRefs))
	resourceLimits, _ := json.Marshal(orEmptyMap(e.resourceLimits))
	outputSchema, _ := json.Marshal(orEmptyMap(e.outputSchema))
	networkPolicy, _ := json.Marshal(orEmptyMap(e.networkPolicy))
	receipt := orEmptyMap(e.receipt)
	if len(e.outputRefs) > 0 {
		receipt["output_refs"] = e.outputRefs
	}
	receiptJSON, _ := json.Marshal(receipt)
	var deadline any
	if !e.deadline.IsZero() {
		deadline = e.deadline
	}
	query := "AppendCapabilityJobEntry"
	args := []any{
		s.mintID("cje"), tenant.Organization, tenant.Project, e.jobID, e.kind, e.idempotencyKey, e.runID,
		e.attemptID, e.workerID, e.capability, e.operation, inputRefs, secretRefs, deadline, resourceLimits,
		outputSchema, networkPolicy, e.sideEffectKey, e.fence, receiptJSON,
	}
	if e.guarded {
		// Leased/terminal legs re-verify the §31.6 fence IN the INSERT (one statement snapshot). A concurrent
		// re-dispatch/health change that has committed makes it 0 rows; if not yet visible, the two appenders
		// compute the same entry_seq and one loses UNIQUE — both surface as errFenceGuardMiss.
		query = "AppendGuardedJobEntry"
		args = append(args, e.workerFence)
	}
	var seq, fence int64
	err := s.pool.QueryRow(sctx, storage.Query(query), args...).Scan(&seq, &fence)
	if e.guarded && (errors.Is(err, pgx.ErrNoRows) || isUniqueViolation(err)) {
		return errFenceGuardMiss
	}
	if err != nil {
		return fmt.Errorf("append job entry (%s): %w", e.kind, err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func decodeStrings(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

func decodeMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func orEmptySlice(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func orEmptyMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return in
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
