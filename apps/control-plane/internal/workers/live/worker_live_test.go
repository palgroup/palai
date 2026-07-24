//go:build live

// Package live is the TWO-SIDED live proof of the CapabilityWorker contract (E17 Task 9, spec §31, WRK-001,
// WRK-002, WRK-003, WRK-005, WRK-006): the control-plane capability gateway (over a REAL PostgreSQL store) on
// one side, and a REAL, separately-executed macOS-native fixture worker PROCESS on the other. The worker is
// built and run as its OWN OS process — outside this test process's memory and outside any container — so it
// is genuinely network-separated (a real private-network posture) and reaches the gateway only OUTBOUND.
//
// It proves, end to end across two processes: a typed swift.build-check round-trip (artifact in ->
// build-check -> artifact out + execution receipt), that the redeemed secret VALUE never appears in the
// worker's output, the no-tunnel structural surface (only four typed routes; anything else 404), and the
// fence-stale-reject (a re-dispatched job's late result is 409). It is self-contained (an httptest gateway +
// a subprocess worker + an operator-provided throwaway Postgres) and tears everything down — no leaked
// containers or volumes.
//
// HONEST CEILING: the "control plane" here is an in-process httptest gateway, not the compose container; the
// container-vs-native framing is the operator leg. And swift.build-check is a type-check, NOT a signed macOS/
// iOS build — no signing credential exists anywhere (apple-build disabled, §6 leg 3).
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/workers"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

const workerCmdPkg = "github.com/palgroup/palai/apps/control-plane/cmd/palai-capability-worker"

func liveURL(t *testing.T) string {
	t.Helper()
	for _, key := range []string{"PALAI_WORKER_LIVE_POSTGRES_URL", "PALAI_COMPONENT_POSTGRES_URL"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	t.Skip("set PALAI_WORKER_LIVE_POSTGRES_URL (or PALAI_COMPONENT_POSTGRES_URL) to run the two-sided worker live proof")
	return ""
}

func liveStore(t *testing.T) (*coordinator.Store, *workers.Store, *workers.Gateway) {
	t.Helper()
	cs, err := coordinator.Open(context.Background(), liveURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	store := workers.NewStore(cs.Pool(), fakeSecrets{vals: map[string]string{"build-cache-token": secretMarker}}, newID, nil)
	return cs, store, workers.NewGateway(store, 5*time.Minute)
}

const secretMarker = "LIVE-SECRET-do-not-leak-4b7e21"

type fakeSecrets struct{ vals map[string]string }

func (f fakeSecrets) Resolve(_ context.Context, _ string, name string) ([]byte, bool, error) {
	v, ok := f.vals[name]
	return []byte(v), ok, nil
}

func newID(prefix string) string { return prefix + "_" + time.Now().Format("150405.000000000") }

func seedTenant(t *testing.T, cs *coordinator.Store) workers.Tenant {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	org, project := newID("org"), newID("prj")
	if _, err := cs.Pool().Exec(ctx, `INSERT INTO organizations (id) VALUES ($1)`, org); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := cs.Pool().Exec(ctx, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return workers.Tenant{Organization: org, Project: project}
}

// buildWorker compiles the fixture worker binary once for the test.
func buildWorker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "palai-capability-worker")
	cmd := exec.Command("go", "build", "-o", bin, workerCmdPkg)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build worker binary: %v\n%s", err, out)
	}
	return bin
}

// TestTwoSidedTypedJobRoundTrip is the crown two-sided proof: a real subprocess worker enrolls outbound,
// claims a swift.build-check job, redeems a job-scoped secret handle WITHOUT echoing it, runs the check, and
// submits an output artifact + receipt. The journal terminalizes 'completed', the output artifact round-trips,
// and neither the worker's stdout nor the journal carries the secret value.
func TestTwoSidedTypedJobRoundTrip(t *testing.T) {
	cs, store, gw := liveStore(t)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	tenant := seedTenant(t, cs)
	bin := buildWorker(t)

	// The control plane provisions a one-time enrollment token + dispatches a typed job with an input
	// artifact and a job-scoped secret handle.
	enrollTok := gw.IssueEnrollmentToken(tenant, "swift-toolchain")
	inputRef := gw.PutInputArtifact([]byte("func add(_ a: Int, _ b: Int) -> Int { return a + b }\n"))
	jobID, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check",
		InputRefs: []string{inputRef}, SecretHandleRefs: []string{"build-cache-token"},
		Deadline: time.Now().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("DispatchJob: %v", err)
	}

	// Run the worker as a SEPARATE OS process — the genuinely-separated native side.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-gateway", srv.URL, "-enroll-token", enrollTok, "-poll-for", "30s")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("worker process failed: %v\nstdout:%s\nstderr:%s", err, stdout.String(), stderr.String())
	}

	// The worker's own output must not leak the secret value.
	if strings.Contains(stdout.String(), secretMarker) || strings.Contains(stderr.String(), secretMarker) {
		t.Fatalf("secret value leaked into the worker process output")
	}
	if !strings.Contains(stdout.String(), "worker completed job_id="+jobID) {
		t.Fatalf("worker did not report completing the job; stdout:\n%s", stdout.String())
	}

	// The journal terminalized completed, the receipt round-tripped, and an output artifact ref is recorded.
	var kind, receipt, refs string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT entry_kind, receipt::text, receipt->'output_refs'->>0 FROM capability_jobs WHERE job_id=$1 ORDER BY entry_seq DESC LIMIT 1`, jobID).
		Scan(&kind, &receipt, &refs); err != nil {
		t.Fatalf("read terminal entry: %v", err)
	}
	if kind != "completed" {
		t.Fatalf("terminal entry = %q, want completed; receipt=%s", kind, receipt)
	}
	// The receipt names the toolchain mode HONESTLY (real-swiftc where swiftc is present, else toy).
	if !strings.Contains(receipt, "real-swiftc") && !strings.Contains(receipt, "toy") {
		t.Fatalf("receipt names no honest toolchain mode: %s", receipt)
	}
	// JSONB re-serializes with spaces; normalize before matching the flag.
	if !strings.Contains(strings.ReplaceAll(receipt, " ", ""), `"secret_redeemed":true`) {
		t.Fatalf("receipt does not confirm a redeemed handle (without its value): %s", receipt)
	}
	if strings.Contains(receipt, secretMarker) {
		t.Fatalf("secret value leaked into the receipt")
	}
	// The output artifact round-trips: its recorded ref resolves to the build-check report bytes.
	if refs == "" {
		t.Fatal("no output artifact ref recorded on the receipt")
	}
	if out, ok := gw.OutputArtifact(refs); !ok || !strings.Contains(string(out), "swift.build-check report") {
		t.Fatalf("output artifact %q did not round-trip: ok=%v", refs, ok)
	}
	t.Logf("two-sided round-trip OK: job=%s toolchain-mode present, output=%s", jobID, refs)
}

// TestNoTunnelSurface is the no-tunnel structural proof (§31.5): the live gateway exposes ONLY the four typed
// routes; any other path is 404, so there is no generic connect/proxy/exec — an ordinary sandbox worker
// cannot be used as a general tunnel. It also confirms the store refuses a dispatch of an untyped operation.
func TestNoTunnelSurface(t *testing.T) {
	cs, store, gw := liveStore(t)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	tenant := seedTenant(t, cs)

	for _, path := range []string{"/capability/connect", "/capability/proxy", "/capability/exec", "/connect", "/"} {
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s status = %d, want 404 (no tunnel route)", path, resp.StatusCode)
		}
	}
	// The store refuses to even dispatch an untyped operation.
	if _, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{Capability: "swift-toolchain", Operation: "tunnel.connect"}); err == nil {
		t.Fatal("dispatch of an untyped operation was accepted; want refused (no tunnel)")
	}
}

// TestFenceStaleRejectThroughGateway proves the §31.6 fence-stale-reject end to end over the HTTP gateway: a
// worker enrolls and claims, the job is re-dispatched (fence+1), and the worker's late result is 409.
func TestFenceStaleRejectThroughGateway(t *testing.T) {
	cs, store, gw := liveStore(t)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	tenant := seedTenant(t, cs)

	enrollTok := gw.IssueEnrollmentToken(tenant, "swift-toolchain")
	workload := enrollOverHTTP(t, srv.URL, enrollTok)

	if _, err := store.DispatchJob(context.Background(), tenant, workers.JobSpec{
		Capability: "swift-toolchain", Operation: "swift.build-check", Deadline: time.Now().Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("DispatchJob: %v", err)
	}
	// The worker claims via the gateway (the gateway now holds the authoritative claim server-side).
	claim := postJSON(t, srv.URL+"/capability/claim", workload, nil)
	jobID, _ := claim["job_id"].(string)
	if jobID == "" {
		t.Fatalf("claim returned no job: %v", claim)
	}
	// A re-dispatch bumps the fence, fencing out the held claim.
	if _, err := store.RedispatchForRetry(context.Background(), tenant, jobID); err != nil {
		t.Fatalf("RedispatchForRetry: %v", err)
	}
	// The worker's late result under the now-stale claim is 409.
	resp := rawPost(t, srv.URL+"/capability/result", workload, `{"job_id":"`+jobID+`","class":"completed","operation":"swift.build-check"}`)
	if resp != http.StatusConflict {
		t.Fatalf("stale result status = %d, want 409 (fence-stale-reject)", resp)
	}
}

// enrollOverHTTP spends an enrollment token over the gateway and returns the workload token.
func enrollOverHTTP(t *testing.T, base, enrollTok string) string {
	t.Helper()
	out := postJSON(t, base+"/capability/enroll", enrollTok, []byte(`{"capability_version":"0.1.0","os":"darwin","arch":"arm64","capacity":1}`))
	tok, _ := out["workload_token"].(string)
	if tok == "" {
		t.Fatalf("enroll returned no workload token: %v", out)
	}
	return tok
}

// postJSON POSTs body with a bearer token and decodes a JSON object response (200).
func postJSON(t *testing.T, url, token string, body []byte) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return map[string]any{}
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

// rawPost POSTs a raw body with a bearer token and returns the status code.
func rawPost(t *testing.T, url, token, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
