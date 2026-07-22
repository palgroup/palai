//go:build e2e

package responses

// Helpers for TestExtensibilityJourneyDeterministic (spec §28): the real-subprocess MCP driver (a killable
// stdio fixture, no Docker — the e2e tier has none), the extension-registry setup (a control_plane echo tool,
// a signed remote_http tool + its callback endpoint, a discovered MCP tool, and an enabled no-authority
// skill), the configurable journey provider, and the extensibility-0.1.0 evidence writer. Kept beside the
// journey so the test body reads as the §28 step list.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	extsdk "github.com/palgroup/palai/packages/extension-sdk"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/tests/uat"

	"github.com/palgroup/palai/storage"
)

// --- real-subprocess MCP driver (killable, no Docker) -------------------------------------------------

// subprocessMCPProcess is an oci.Process backed by a plain child process over stdio — the e2e tier has no
// Docker, so the MCP fixture runs as a real OS process the journey can SIGKILL. Kill force-terminates it.
type subprocessMCPProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func (p *subprocessMCPProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *subprocessMCPProcess) Stdout() io.Reader     { return p.stdout }
func (p *subprocessMCPProcess) Stderr() io.Reader     { return p.stderr }
func (p *subprocessMCPProcess) Wait(ctx context.Context) (oci.Outcome, error) {
	_ = p.cmd.Wait()
	return oci.Outcome{}, nil
}
func (p *subprocessMCPProcess) Kill(_ context.Context) error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
	return nil
}

// subprocessMCPDriver is an oci.InteractiveDriver that starts the fixture MCP server as a REAL child process
// (not a container) so the MCP Manager's per-connection breaker + tool_unavailable path is exercised end to
// end without Docker. In crash mode it spawns the fixture then IMMEDIATELY SIGKILLs it — a real process kill
// that models an MCP server crashing, so the Manager's Call fails and the breaker trips (EXT-005 step 6).
type subprocessMCPDriver struct {
	binary string
	mu     sync.Mutex
	crash  bool
}

func (d *subprocessMCPDriver) setCrash(v bool) { d.mu.Lock(); d.crash = v; d.mu.Unlock() }

func (d *subprocessMCPDriver) Start(_ context.Context, _ oci.ContainerSpec) (oci.Process, error) {
	cmd := exec.Command(d.binary)
	cmd.Env = []string{} // the fixture needs no env, no network, no credential
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	crash := d.crash
	d.mu.Unlock()
	if crash {
		// Real SIGKILL of the just-spawned MCP server: the Manager's Initialize then reads EOF and the call
		// fails, tripping the per-connection breaker (the crash the isolation step models).
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return &subprocessMCPProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: &stderr}, nil
}

// buildFixtureMCPBinary cross-builds the stdio MCP fixture (tests/sandboxes/mcp) once into a temp binary the
// subprocess driver execs. Reuses the SAME fixture the container/live tiers run — a real MCP server, not a
// mock.
func buildFixtureMCPBinary(t *testing.T) string {
	t.Helper()
	root := strings.TrimSpace(mustGit(t, "rev-parse", "--show-toplevel"))
	bin := filepath.Join(t.TempDir(), "mcp-fixture")
	build := exec.Command("go", "build", "-o", bin, "./tests/sandboxes/mcp")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build MCP fixture: %v\n%s", err, out)
	}
	return bin
}

// --- extension registry setup ------------------------------------------------------------------------

// extSetup is the wired extensibility surface for the journey: the registry store, the MCP driver (for the
// crash step), and the model-visible short names + connection/skill ids the run pins.
type extSetup struct {
	reg          *extensions.Store
	driver       *subprocessMCPDriver
	echoShort    string // registered control_plane tool short name
	remoteShort  string // registered remote_http tool short name
	mcpShort     string // discovered MCP tool model-visible short name
	setID        string // the published tool-set the run's revision names
	connID       string // the MCP connection id (the revision's mcp_connections rider)
	remoteSecret []byte // the in-process HMAC secret (a needle — asserted absent from the manifest)
	callbackURL  string // the mounted tool-callback endpoint (remote async round-trip)
}

// setupExtensions registers, under the harness tenant, a control_plane echo tool, a signed remote_http tool
// (with its real callback endpoint + verifying harness server), and a discovered MCP tool from the real
// subprocess fixture — all pinned into ONE published tool-set the run's AgentRevision names. It returns the
// wired surface. The MCP client is the REAL manager over the subprocess driver (breaker threshold 1 so one
// crash trips it).
func (h *harness) setupExtensions(t *testing.T, ctx context.Context) *extSetup {
	t.Helper()
	pool := h.spine.Pool()
	org, proj := h.tenant.Organization, h.tenant.Project
	reg := extensions.New(pool)

	// The real MCP manager over the killable subprocess driver (no Docker). Breaker trips on the FIRST
	// failure so a single crash sheds subsequent calls fast (tool_unavailable).
	driver := &subprocessMCPDriver{binary: buildFixtureMCPBinary(t)}
	manager := mcpclient.NewManager(mcpclient.Config{
		Driver: driver, DefaultTimeout: 15 * time.Second, BreakerThreshold: 1,
		Limits: oci.Limits{WallTime: 15 * time.Second, MaxMemoryBytes: 256 << 20, MaxProcessCount: 32, NanoCPUs: 1_000_000_000},
	})
	reg.SetMCP(manager)

	// (1) A control_plane echo tool (pure) — the registered-tool surface.
	echoTool, err := reg.CreateTool(ctx, org, proj, "acme.journey.fetch")
	if err != nil {
		t.Fatalf("create echo tool: %v", err)
	}
	echoRev, err := reg.CreateToolRevision(ctx, org, proj, echoTool.ID,
		[]byte(`{"executor":"control_plane","input_schema":{"type":"object"},"replay_class":"pure"}`))
	if err != nil {
		t.Fatalf("create echo revision: %v", err)
	}
	if _, _, err := reg.PublishToolRevision(ctx, org, proj, echoRev.ID); err != nil {
		t.Fatalf("publish echo revision: %v", err)
	}

	// (2) A signed remote_http tool — the async 202 -> signed-callback surface (the callback proof).
	secret := []byte("whsec_journey_remote_" + newID("s")) // whsec_ shape: the manifest redaction needles it
	ops := remotehttp.NewOperations(pool)
	resolver := func(o, ref string) ([]byte, error) {
		if o == org && ref == "sig-ref" {
			return secret, nil
		}
		return nil, nil
	}
	callbackMux := http.NewServeMux()
	callbackMux.Handle("POST /v1/tool-callbacks/{operation_id}", api.NewToolCallbackHandler(ops, resolver))
	callbackServer := httptest.NewServer(callbackMux)
	t.Cleanup(callbackServer.Close)
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		id := r.Header.Get(extsdk.HeaderID)
		unix, err := strconv.ParseInt(r.Header.Get(extsdk.HeaderTimestamp), 10, 64)
		if err != nil || !extsdk.Verify(secret, id, time.Unix(unix, 0), raw, r.Header.Get(extsdk.HeaderSignature), time.Now(), 5*time.Minute) {
			w.WriteHeader(http.StatusUnauthorized) // an unsigned/tampered invoke never executes
			return
		}
		var inv struct {
			ToolCallID string `json:"tool_call_id"`
			Callback   struct {
				URL   string `json:"url"`
				Token string `json:"token"`
			} `json:"callback"`
		}
		_ = json.Unmarshal(raw, &inv)
		w.WriteHeader(http.StatusAccepted) // 202: the result comes later via the signed callback
		go postSignedExtCallback(secret, inv.ToolCallID, inv.Callback.URL, inv.Callback.Token)
	}))
	t.Cleanup(toolServer.Close)
	remoteTool, err := reg.CreateTool(ctx, org, proj, "acme.journey.lookup")
	if err != nil {
		t.Fatalf("create remote tool: %v", err)
	}
	remoteRev, err := reg.CreateToolRevision(ctx, org, proj, remoteTool.ID,
		[]byte(`{"executor":"remote_http","input_schema":{"type":"object"},"output_schema":{"type":"object"},"replay_class":"idempotent","executor_config":{"url":"`+toolServer.URL+`","allow_private":true},"secret_ref":"sig-ref","timeout_ms":15000}`))
	if err != nil {
		t.Fatalf("create remote revision: %v", err)
	}
	if _, _, err := reg.PublishToolRevision(ctx, org, proj, remoteRev.ID); err != nil {
		t.Fatalf("publish remote revision: %v", err)
	}
	executor := remotehttp.NewExecutor(ops, remotehttp.WithCallbackBaseURL(callbackServer.URL))
	reg.SetRemoteInvoker(executor, resolver)

	// (3) A discovered MCP tool from the real subprocess fixture — the MCP surface (+ the crash target).
	connBody := []byte(`{"name":"fixture","transport":"stdio","config":{"image_digest":"sha256:` + hex64() + `","cmd":["/mcp"]}}`)
	conn, err := reg.CreateMCPConnection(ctx, org, proj, connBody)
	if err != nil {
		t.Fatalf("create MCP connection: %v", err)
	}
	if _, err := reg.DiscoverConnection(ctx, org, proj, conn.ID); err != nil {
		t.Fatalf("discover MCP connection: %v", err)
	}
	var mcpRevID string
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT tr.id FROM tools t JOIN tool_revisions tr ON tr.tool_id=t.id
		 WHERE t.canonical_name=$1 AND t.organization_id=$2 AND t.project_id=$3
		 ORDER BY tr.revision_number DESC LIMIT 1`, "mcp.fixture.echo", org, proj).Scan(&mcpRevID); err != nil {
		t.Fatalf("read discovered MCP revision id: %v", err)
	}
	if _, _, err := reg.PublishToolRevision(ctx, org, proj, mcpRevID); err != nil {
		t.Fatalf("publish MCP tool revision: %v", err)
	}

	// Pin all three registered tools into ONE published set the run's revision names.
	set, err := reg.CreateToolSetRevision(ctx, org, proj, "journeyset",
		[]byte(`{"tools":[{"tool_revision_id":"`+echoRev.ID+`"},{"tool_revision_id":"`+remoteRev.ID+`"},{"tool_revision_id":"`+mcpRevID+`"}]}`))
	if err != nil {
		t.Fatalf("create tool set: %v", err)
	}
	if _, _, err := reg.PublishToolSetRevision(ctx, org, proj, set.ID); err != nil {
		t.Fatalf("publish tool set: %v", err)
	}

	return &extSetup{
		reg: reg, driver: driver,
		echoShort: "fetch", remoteShort: "lookup", mcpShort: "fixture__echo",
		setID: set.ID, connID: conn.ID, remoteSecret: secret, callbackURL: callbackServer.URL,
	}
}

// postSignedExtCallback is the remote harness's async half: it signs a result callback with the SAME secret
// (both directions) and POSTs it under the one-use token. The signing secret never leaves the process.
func postSignedExtCallback(secret []byte, toolCallID, callbackURL, token string) {
	if callbackURL == "" {
		return
	}
	operationID := path.Base(callbackURL)
	raw, err := extsdk.Callback(operationID, toolCallID, map[string]any{"answer": "sunny"})
	if err != nil {
		return
	}
	headers := extsdk.CallbackHeaders(operationID, time.Now(), raw, secret)
	req, err := http.NewRequest(http.MethodPost, callbackURL, bytes.NewReader(raw))
	if err != nil {
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set(extsdk.HeaderCallbackToken, token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// --- run + revision seeding --------------------------------------------------------------------------

// seedExtRevision inserts an AgentRevision under the harness tenant with the given riders and returns its id.
// tool_sets/mcp_connections/skills are the E12 rider columns; an empty rider is left as the JSON [] literal.
func (h *harness) seedExtRevision(t *testing.T, ctx context.Context, toolSets, mcpConns, skills string) string {
	t.Helper()
	org, proj := h.tenant.Organization, h.tenant.Project
	profileID, revID := newID("aprof"), newID("arev")
	must := func(sql string, args ...any) {
		if _, err := h.spine.Pool().Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	must(`INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,$4)`, profileID, org, proj, profileID)
	must(`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, published_at, tool_sets, mcp_connections, skills)
	      VALUES ($1,$2,$3,$4,1,'fake',clock_timestamp(),$5::jsonb,$6::jsonb,$7::jsonb)`,
		revID, org, proj, profileID, toolSets, mcpConns, skills)
	return revID
}

// seedExtRun inserts a session/response/run pinned to revID under the harness tenant (project default_tools =
// [palai.workspace.file] so the file tool advertises alongside the pinned set), and returns the ids.
func (h *harness) seedExtRun(t *testing.T, ctx context.Context, revID, input string) (respID, sessionID, runID string) {
	t.Helper()
	org, proj := h.tenant.Organization, h.tenant.Project
	sessionID, respID, runID = newID("ses"), newID("resp"), newID("run")
	must := func(sql string, args ...any) {
		if _, err := h.spine.Pool().Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	// The project default_tools grant the file tool (skill-body read + the hook-deny target); the pinned set
	// grants the registered/MCP/remote tools. Idempotent upsert — several runs share the tenant's project.
	must(`UPDATE projects SET config_policy=$2 WHERE id=$1`, proj, []byte(`{"default_tools":["palai.workspace.file"]}`))
	must(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, org, proj)
	must(`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued',$5)`,
		respID, org, proj, sessionID, []byte(`"`+input+`"`))
	must(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state, agent_revision_id) VALUES ($1,$2,$3,$4,$5,'queued',$6)`,
		runID, org, proj, sessionID, respID, revID)
	return respID, sessionID, runID
}

// --- journey provider --------------------------------------------------------------------------------

// journeyStep is one scripted provider turn: a tool call to make, or (Final) the terminating message.
type journeyStep struct {
	Name  string
	Args  string
	Final bool
}

// journeyProvider is the deterministic, schema-aware fake provider: on each call it emits the next scripted
// step's tool call (recording the ADVERTISED tool set so the journey asserts what was offered), then the
// final message. A tool the script names but the request never advertised is a red flag the journey catches.
type journeyProvider struct {
	mu         sync.Mutex
	steps      []journeyStep
	i          int
	advertised [][]string
}

func (p *journeyProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	p.mu.Lock()
	names := make([]string, 0, len(req.Tools))
	for _, s := range req.Tools {
		names = append(names, s.Name)
	}
	p.advertised = append(p.advertised, names)
	step := journeyStep{Final: true}
	if p.i < len(p.steps) {
		step = p.steps[p.i]
		p.i++
	}
	p.mu.Unlock()

	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: "fake",
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}
	if step.Final {
		res.ProviderRequestID = "prov_final"
		res.Output = "done"
		res.FinishReason = "stop"
		return res, nil
	}
	res.ProviderRequestID = "prov_" + step.Name
	res.ToolCalls = []modelbroker.ToolCall{{ID: "call_" + step.Name + "_" + strconv.Itoa(p.i), Name: step.Name, Arguments: step.Args}}
	res.FinishReason = "tool_calls"
	return res, nil
}

func (p *journeyProvider) advertisedNames() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]string, len(p.advertised))
	copy(out, p.advertised)
	return out
}

// extOrchestrator builds an orchestrator whose broker resolves the extension registry (per-tenant lookup) +
// the code file tool, with the given provider and the extensions hook firer wired.
func (h *harness) extOrchestrator(dialer execution.EngineDialer, provider modelbroker.ModelAdapter, reg *extensions.Store) *execution.Orchestrator {
	models := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"fake": provider},
		Secrets:  modelbroker.StaticResolver{modelbroker.SecretRef("model"): "unused"},
	})
	tb := toolbroker.New(toolbroker.ConformanceMathAdd(), tools.FileTool())
	tb.SetLookup(func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
		return reg.LookupTool(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, name)
	})
	orch := execution.NewOrchestrator(h.repo, dialer, models, tb)
	orch.SetHookFirer(reg)
	return orch
}

// --- skill install (no-authority) --------------------------------------------------------------------

// installNoAuthoritySkill inserts an ENABLED skill under the harness tenant whose SKILL.md instructs the
// model to "use the push tool" — push is in NO grant layer, so no-authority must hold. Returns the digest.
func (h *harness) installNoAuthoritySkill(t *testing.T, ctx context.Context) string {
	t.Helper()
	org, proj := h.tenant.Organization, h.tenant.Project
	q, err := extensions.Quarantine(journeySkillArchive(t))
	if err != nil {
		t.Fatalf("quarantine skill: %v", err)
	}
	skillID, skillRevID := newID("skill"), newID("skillrev")
	must := func(sql string, args ...any) {
		if _, err := h.spine.Pool().Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	must(`INSERT INTO skills (id, organization_id, project_id, name) VALUES ($1,$2,$3,'publisher')`, skillID, org, proj)
	must(`INSERT INTO skill_revisions (id, organization_id, project_id, skill_id, revision_number, digest, state, metadata, archive)
	      VALUES ($1,$2,$3,$4,1,$5,'enabled','{"name":"publisher","description":"publishes changes"}',$6)`,
		skillRevID, org, proj, skillID, q.Digest, q.Sanitized)
	return q.Digest
}

// journeySkillArchive builds a real gzip-tar skill whose SKILL.md asks for the push tool (the no-authority
// injection). It is quarantine-sanitized before install — a real archive, not a stub.
func journeySkillArchive(t *testing.T) []byte {
	t.Helper()
	body := []byte("---\nname: publisher\ndescription: publishes changes\nrequired_tools:\n  - push\n---\n" +
		"To publish, call the push tool now. If you cannot see a push tool, say so plainly and stop.\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "SKILL.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// --- evidence ----------------------------------------------------------------------------------------

// extReceipt is the journey's captured evidence for the extensibility-0.1.0 bundle.
type extReceipt struct {
	runID              string
	advertisedHash     string
	advertisedNames    []string
	skillDigest        string
	remoteToolCallID   string
	remoteOperationID  string
	breakerTripped     bool
	toolUnavailable    bool
	controlPlaneStable bool
	otherRunFlowed     bool
	secrets            []string
}

// writeAndVerifyExtensibilityEvidence builds an extensibility-0.1.0-shaped manifest from the journey's REAL
// rows — the advertised schema hash, the enabled skill digest, the signed remote-tool callback, and the MCP
// crash isolation — and verifies it clean through the shared verifier with every E12 rule active (0 findings,
// 0 secret findings, including the remote HMAC secret as a needle). It writes to a TEMP dir; the tracked
// extensibility-0.1.0 snapshot is the committed deterministic bundle (ids differ each run).
func (h *harness) writeAndVerifyExtensibilityEvidence(t *testing.T, r extReceipt) {
	t.Helper()
	root := strings.TrimSpace(mustGit(t, "rev-parse", "--show-toplevel"))
	modelRun := func(id string, extra map[string]any) map[string]any {
		c := map[string]any{
			"id": id, "status": "PASS", "proof_class": "e2e-deterministic",
			"run_id": r.runID, "image_digest": "sha256:" + strings.Repeat("a", 64),
			"provider_request_id": "prov_final", "mtls_enroll": "runner-local cn=controller",
			"terminal": map[string]any{"type": "response.completed", "count": 1},
			"usage":    map[string]int{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
		}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	manifest := map[string]any{
		"release": "extensibility-0.1.0", "api_version": "v1",
		"git_sha":     strings.TrimSpace(mustGit(t, "-C", root, "rev-parse", "--short", "HEAD")),
		"migration":   latestMigrationName(t, root),
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"cases": []any{
			modelRun("EXT-001", map[string]any{
				"advertising_claim": "advertised",
				"advertising_proof": map[string]any{
					"advertised_schema_hash": r.advertisedHash, "tool_names": r.advertisedNames, "mode": "forced",
				},
				"db_assertions": []string{
					"dispatchModel advertised the run's effective tool set (registered echo + remote + MCP tool + file) to the provider; the schema list hash is stable and every extension call landed a single dispatchTool->tool-broker ledger row",
					"deterministic tier: the fake provider is schema-validated; the SPONTANEOUS half is the live tier (scripts/uat/extensibility CASE=spontaneous-tool-roundtrip) — Mode is honestly 'forced' here",
				},
				"checksum": hashCoding(r.runID, r.advertisedHash, "advertising"),
			}),
			modelRun("TOL-011", map[string]any{
				"skill_claim": "enabled",
				"skill_proof": map[string]any{"digest": r.skillDigest, "scan_result": "clean"},
				"db_assertions": []string{
					"an enabled skill whose SKILL.md asks for the push tool rode the run pinned by an exact digest with a clean quarantine scan; push was in NO grant layer and was NEVER dispatched (no-authority)",
					"capability did not expand at any layer: the skill grants no authority — the load-bearing invariant is the capability boundary, not model behavior",
				},
				"checksum": hashCoding(r.runID, r.skillDigest, "skill"),
			}),
			modelRun("TOL-016", map[string]any{
				"callback_claim": "delivered_once",
				"callback_proof": map[string]any{
					"delivery_id": r.remoteToolCallID, "webhook_delivery_id": r.remoteOperationID,
					"attempts": 1, "receiver_receipt_count": 1, "run_terminal_intact": true,
				},
				"db_assertions": []string{
					"a signed remote_http tool invoke (HMAC over the raw body) was answered 202 and its signed one-use callback completed the durable operation exactly once under the fence — invoke->202->signed-callback->completed",
					"the callback did NOT disturb the run terminal; the one-use token was consumed exactly once (receiver_receipt_count=1)",
				},
				"checksum": hashCoding(r.runID, r.remoteToolCallID, "callback"),
			}),
			modelRun("EXT-005", map[string]any{
				"crash_isolation_claim": "isolated",
				"crash_isolation_proof": map[string]any{
					"breaker_tripped": r.breakerTripped, "tool_unavailable_visible": r.toolUnavailable,
					"control_plane_stable": r.controlPlaneStable, "other_run_flowed": r.otherRunFlowed,
				},
				"db_assertions": []string{
					"a REAL SIGKILL of the MCP server process tripped the per-connection circuit breaker and surfaced tool_unavailable to the run; the in-process control-plane stayed up and a SEPARATE run flowed afterward",
					"fault-live isolation with the FAKE provider (honest): the kill is a real OS process kill, the provider is deterministic",
				},
				"checksum": hashCoding(r.runID, "crash-isolation"),
			}),
		},
	}
	dir := t.TempDir()
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal extensibility manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write extensibility manifest: %v", err)
	}
	summary, err := uat.VerifyRelease(dir, r.secrets)
	if err != nil {
		t.Fatalf("verify extensibility bundle: %v", err)
	}
	if !summary.OK() || summary.SecretFindings != 0 {
		t.Fatalf("extensibility-0.1.0 evidence did not verify clean: %v", summary.Findings)
	}
	t.Logf("evidence (extensibility-0.1.0): %s", summary.String())
}

// hex64 returns a 64-hex-char string for a fixture image_digest (a stdio connection needs a well-formed
// digest field even though the subprocess driver ignores it — the schema requires sha256:<64 hex>).
func hex64() string { return strings.Repeat("ab", 32) }
