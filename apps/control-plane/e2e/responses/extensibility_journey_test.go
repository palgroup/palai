//go:build e2e

package responses

// TestExtensibilityJourneyDeterministic is the E12 Task 10 deterministic half of the mandatory extensibility
// integration journey (spec §28, plan §5 Task 10). It composes the extensibility spine end to end in CI with
// NO network beyond localhost and NO real credential: the FAKE provider (schema-validated) drives the REAL
// registry + MCP client (a REAL subprocess stdio fixture) + signed remote_http transport + skill + hook seams
// + orchestrator + reference engine against a throwaway Postgres. It proves, in one tenant, that:
//
//   - a registered control_plane tool, a discovered MCP tool, and a signed remote_http tool (202 -> signed
//     one-use callback) each flow through the SINGLE dispatchTool -> tool-broker ledger path (single admission);
//   - capability expands at NO layer: an enabled skill whose SKILL.md asks for the push tool never puts push
//     in the advertised set and never dispatches it (no-authority);
//   - a before_tool policy hook DENIES a tool call VISIBLY (policy.denied.v1, no executed effect);
//   - a REAL SIGKILL of the MCP server process trips the per-connection breaker + surfaces tool_unavailable,
//     the in-process control-plane stays up, and a SEPARATE run flows afterward (EXT-005, the E12 exit gate);
//   - then a terminal run + self-verified evidence.
//
// It lives in package responses (not the plan's literal tests/e2e/extensions) because it drives the control
// plane's internal execution + extensions packages, which Go's internal rule forbids importing from tests/ —
// the SAME constraint that put the E08 newHarness and the E11 automation journey here.
//
// HONEST CEILINGS (spec §10.2): the provider is the deterministic fake (schema-validated), so the tool CHOICE
// is scripted, not spontaneous — the SPONTANEOUS half is the live tier (scripts/uat/extensibility). The MCP +
// remote + hook servers are our fixtures; what this tier proves is the real dispatch/ledger/breaker/isolation
// machinery, with a REAL OS-process kill for the crash step.

import (
	"context"
	"errors"
	"testing"

	mcpclient "github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

func TestExtensibilityJourneyDeterministic(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// --- Step 1: register the extensibility surface under the tenant (a control_plane echo tool, a signed
	// remote_http tool + its callback endpoint, a discovered MCP tool from a REAL subprocess fixture), pin all
	// three into a published set, and enable a no-authority skill. ---
	ext := h.setupExtensions(t, ctx)
	skillDigest := h.installNoAuthoritySkill(t, ctx)

	revExt := h.seedExtRevision(t, ctx, `["`+ext.setID+`"]`, `["`+ext.connID+`"]`, `["publisher"]`)
	resp1, session1, run1 := h.seedExtRun(t, ctx, revExt, "Use the registered tools and the skill.")
	alloc1 := newAllocationRoot(t)
	if err := h.repo.PinRunSkills(ctx, h.tenant, run1); err != nil {
		t.Fatalf("pin run skills: %v", err)
	}
	if err := h.repo.MaterializeRunSkills(ctx, h.tenant, run1, alloc1); err != nil {
		t.Fatalf("materialize run skills: %v", err)
	}

	// --- Step 2 + 3: run 1 executes; the model sees the ADVERTISED effective set and calls the registered
	// echo, the MCP tool, and the remote tool — each landing a single dispatchTool -> tool-broker ledger row. ---
	prov1 := &journeyProvider{steps: []journeyStep{
		{Name: ext.echoShort, Args: `{"query":"hello"}`},
		{Name: ext.mcpShort, Args: `{"message":"hello mcp"}`},
		{Name: ext.remoteShort, Args: `{"query":"weather"}`},
	}}
	orch1 := h.extOrchestrator(subprocessDialer{engineDir: h.engineDir}, prov1, ext.reg)
	if err := orch1.ExecuteAttempt(ctx, h.workspaceDescriptor(run1, 1, alloc1)); err != nil {
		t.Fatalf("run 1 (extensions) execute: %v", err)
	}
	if state, _ := h.response(resp1); state != "completed" {
		t.Fatalf("run 1 state = %q, want completed", state)
	}

	// The advertised set carried the three registered tools + the file tool, and NEVER push (the skill asked
	// for push; capability did not expand).
	advertised := prov1.advertisedNames()
	if len(advertised) == 0 {
		t.Fatal("run 1: the provider was never called")
	}
	for _, want := range []string{ext.echoShort, ext.mcpShort, ext.remoteShort, "palai.workspace.file"} {
		if !advertisedEver(advertised, want) {
			t.Fatalf("run 1: %q was never advertised; advertised sets = %v", want, advertised)
		}
	}
	if advertisedEver(advertised, "push") {
		t.Fatalf("run 1: the push tool was advertised despite the skill's request — capability expanded (no-authority breached): %v", advertised)
	}

	// Single-admission: each extension call landed EXACTLY ONE completed tool_calls ledger row (no second
	// dispatch loop). The registered echo + the remote tool + the MCP tool each executed once.
	for _, name := range []string{ext.echoShort, ext.remoteShort, ext.mcpShort} {
		if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name=$2 AND state='completed'`, run1, name); n != 1 {
			t.Fatalf("run 1: completed tool_calls for %q = %d, want exactly 1 (single-admission ledger)", name, n)
		}
	}
	// No-authority: the skill's requested push tool was NEVER dispatched.
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='push'`, run1); n != 0 {
		t.Fatalf("run 1: push tool_calls = %d, want 0 (a skill grants no authority)", n)
	}

	// The signed remote_http round-trip completed via the one-use callback (invoke -> 202 -> signed callback).
	var remoteOpID, remoteCallID, remoteOpState string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT id, tool_call_id, state FROM remote_tool_operations WHERE organization_id=$1 AND project_id=$2 ORDER BY created_at DESC LIMIT 1`,
		h.tenant.Organization, h.tenant.Project).Scan(&remoteOpID, &remoteCallID, &remoteOpState); err != nil {
		t.Fatalf("read remote_tool_operations: %v", err)
	}
	if remoteOpState != "completed" {
		t.Fatalf("remote operation state = %q, want completed (signed async round-trip)", remoteOpState)
	}

	// --- Step 6 (crash): a REAL SIGKILL of the MCP server process. Run 2 calls the MCP tool post-kill; the
	// call fails, the per-connection breaker trips, and the run sees tool_unavailable — the control-plane
	// stays up. ---
	ext.driver.setCrash(true)
	revCrash := h.seedExtRevision(t, ctx, `["`+ext.setID+`"]`, `["`+ext.connID+`"]`, `[]`)
	resp2, _, run2 := h.seedExtRun(t, ctx, revCrash, "Call the MCP tool.")
	prov2 := &journeyProvider{steps: []journeyStep{{Name: ext.mcpShort, Args: `{"message":"after crash"}`}}}
	orch2 := h.extOrchestrator(subprocessDialer{engineDir: h.engineDir}, prov2, ext.reg)
	// Positive proof the surface was EXERCISED (not merely absent): ExecuteAttempt must FAIL — the MCP tool
	// was attempted after the real kill and its dispatch errored. A nil here would mean the tool was never
	// dispatched (a seed/advertising regression), so this catches that.
	err2 := orch2.ExecuteAttempt(ctx, h.workspaceDescriptor(run2, 1, newAllocationRoot(t)))
	if err2 == nil {
		t.Fatal("run 2: ExecuteAttempt succeeded after a real SIGKILL — the MCP tool was not attempted or did not fail (the crash surface was not exercised)")
	}
	t.Logf("run 2 (crash) execute returned %v (expected — the MCP tool was attempted and failed on the real kill)", err2)
	// The MCP tool WAS advertised to run 2 (offered to the model), and the run saw tool_unavailable: no
	// completed MCP ledger row — the dispatch failed, visibly.
	if !advertisedEver(prov2.advertisedNames(), ext.mcpShort) {
		t.Fatalf("run 2: the MCP tool %q was never advertised — the crash surface was not exercised", ext.mcpShort)
	}
	toolUnavailableVisible := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name=$2 AND state='completed'`, run2, ext.mcpShort) == 0
	if !toolUnavailableVisible {
		t.Fatalf("run 2: the MCP tool COMPLETED after a real SIGKILL — the crash was not surfaced as tool_unavailable")
	}
	// The breaker TRIPPED: turn the fixture HEALTHY AGAIN (crash off), then a fresh dispatch is STILL shed
	// with ErrToolUnavailable — that sentinel returns ONLY from an OPEN breaker, BEFORE any dial. With the
	// server healthy the call would otherwise succeed, so this proves the breaker is open (not merely that
	// one call failed): if the breaker were deleted, the healthy probe would succeed and this assert would fail.
	ext.driver.setCrash(false)
	probeBroker := toolbroker.New()
	probeBroker.SetLookup(func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
		return ext.reg.LookupTool(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, name)
	})
	probeEnv := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: h.tenant.Organization, Project: h.tenant.Project, RunID: run2}}
	_, probeErr := probeBroker.Execute(ctx, contracts.ToolCallID("tc_breaker_probe"), ext.mcpShort, map[string]any{"message": "shed"}, 2, probeEnv)
	breakerTripped := errors.Is(probeErr, mcpclient.ErrToolUnavailable)
	if !breakerTripped {
		t.Fatalf("run 2: a post-crash MCP dispatch was not shed with ErrToolUnavailable (got %v) — the circuit breaker did not trip (a deleted breaker would let the now-healthy server succeed)", probeErr)
	}

	// --- Step 5 (hook deny) + the crash-isolation "other run flowed" proof: register a before_tool policy
	// hook, then run 3 (POST-crash). The control-plane processes it — proving it stayed up — and the hook
	// DENIES the file tool visibly (policy.denied.v1, no executed effect). ---
	if _, err := ext.reg.CreateHook(ctx, h.tenant.Organization, h.tenant.Project,
		[]byte(`{"name":"deny-tools","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"deny_all"}}`)); err != nil {
		t.Fatalf("register before_tool deny hook: %v", err)
	}
	revHook := h.seedExtRevision(t, ctx, `[]`, `[]`, `[]`)
	resp3, session3, run3 := h.seedExtRun(t, ctx, revHook, "Write a file.")
	prov3 := &journeyProvider{steps: []journeyStep{{Name: "palai.workspace.file", Args: `{"op":"write","path":"blocked.txt","content":"x\n"}`}}}
	orch3 := h.extOrchestrator(subprocessDialer{engineDir: h.engineDir}, prov3, ext.reg)
	if err := orch3.ExecuteAttempt(ctx, h.workspaceDescriptor(run3, 1, newAllocationRoot(t))); err != nil {
		t.Logf("run 3 (hook deny) execute returned %v (tolerated — the deny evidence is in the DB)", err)
	}
	// The control-plane processed run 3 after the MCP crash (it did not fall) — the "other run flowed" fact.
	st3, _ := h.response(resp3)
	otherRunFlowed := st3 != ""
	if !otherRunFlowed {
		t.Fatal("run 3 never reached the control-plane after the crash — the process did not stay up")
	}
	// The before_tool policy hook denied the file tool: a real control-plane deny fired, and the tool never ran.
	if n := h.count(`SELECT count(*) FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type='policy.denied.v1'`,
		session3, h.tenant.Organization, h.tenant.Project); n < 1 {
		t.Fatalf("run 3: no policy.denied.v1 journaled — the before_tool hook deny never fired")
	}
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='palai.workspace.file' AND state='completed'`, run3); n != 0 {
		t.Fatalf("run 3: a denied file tool executed (%d completed rows), want 0 — a deny must never run the effect", n)
	}

	// --- Step 7: pass + self-verified evidence. The four E12 rules (advertising / skill / callback / crash-
	// isolation) are exercised on the journey's REAL rows; the remote HMAC secret is a needle. ---
	_ = session1
	_ = resp2
	h.writeAndVerifyExtensibilityEvidence(t, extReceipt{
		runID:              run1,
		advertisedHash:     hashCoding(ext.echoShort, ext.mcpShort, ext.remoteShort, "palai.workspace.file"),
		advertisedNames:    []string{ext.echoShort, ext.mcpShort, ext.remoteShort, "palai.workspace.file"},
		skillDigest:        skillDigest,
		remoteToolCallID:   remoteCallID,
		remoteOperationID:  remoteOpID,
		breakerTripped:     breakerTripped,
		toolUnavailable:    toolUnavailableVisible,
		controlPlaneStable: otherRunFlowed,
		otherRunFlowed:     otherRunFlowed,
		secrets:            []string{string(ext.remoteSecret)},
	})
}

// advertisedEver reports whether name appeared in any of the per-call advertised tool sets.
func advertisedEver(sets [][]string, name string) bool {
	for _, set := range sets {
		for _, n := range set {
			if n == name {
				return true
			}
		}
	}
	return false
}
