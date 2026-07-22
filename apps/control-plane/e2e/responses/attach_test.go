//go:build e2e

package responses

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"

	"github.com/palgroup/palai/storage"
)

// attachFrame is one SSE frame reduced to what SES-001 compares: the per-session event
// id and a hash of the CloudEvents payload. Two authorized clients attaching to one
// session must observe the identical (id, payloadHash) sequence.
type attachFrame struct {
	id          string
	typ         string
	seq         int
	payloadHash string
}

// attachStream is a live SSE reader over the in-process httptest server. close disconnects
// it (the server sees the request context fire); the journal is unaffected by a read.
type attachStream struct {
	cancel context.CancelFunc
	body   io.ReadCloser
	sc     *bufio.Scanner
}

// openAttach issues the streaming GET with the harness credential and an optional header
// set (e.g. Last-Event-ID), returning after the 200 headers arrive.
func (h *harness) openAttach(sessionID string, headers map[string]string) *attachStream {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/v1/sessions/"+sessionID+"/events", nil)
	if err != nil {
		cancel()
		h.t.Fatalf("build attach GET error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		h.t.Fatalf("attach GET error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		h.t.Fatalf("attach status = %d, want 200", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &attachStream{cancel: cancel, body: resp.Body, sc: sc}
}

func (c *attachStream) close() {
	c.cancel()
	c.body.Close()
}

// next returns the next parsed frame, skipping heartbeat comment lines. ok is false at a
// clean close (the server ends the stream after the terminal event). The payload hash is
// over the CloudEvents data object (its JSON marshals with sorted keys, so the same durable
// row hashes identically for every client).
func (c *attachStream) next(t *testing.T) (attachFrame, bool) {
	t.Helper()
	var f attachFrame
	var dataLine string
	got := false
	for c.sc.Scan() {
		line := c.sc.Text()
		switch {
		case line == "":
			if got {
				var env contracts.Event
				if err := json.Unmarshal([]byte(dataLine), &env); err != nil {
					t.Fatalf("decode envelope %q error = %v", dataLine, err)
				}
				f.seq = env.Sequence
				payload, err := json.Marshal(env.Data)
				if err != nil {
					t.Fatalf("marshal payload error = %v", err)
				}
				sum := sha256.Sum256(payload)
				f.payloadHash = hex.EncodeToString(sum[:])
				return f, true
			}
		case strings.HasPrefix(line, ":"):
			// heartbeat comment — ignore
		case strings.HasPrefix(line, "id: "):
			f.id = strings.TrimPrefix(line, "id: ")
			got = true
		case strings.HasPrefix(line, "event: "):
			f.typ = strings.TrimPrefix(line, "event: ")
			got = true
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
			got = true
		}
	}
	return attachFrame{}, false
}

// drain reads every remaining frame through the clean close.
func (c *attachStream) drain(t *testing.T) []attachFrame {
	t.Helper()
	var out []attachFrame
	for {
		f, ok := c.next(t)
		if !ok {
			return out
		}
		out = append(out, f)
	}
}

// TestTwoClientsSeeIdenticalOrderedJournal proves two concurrent authorized SSE readers on
// one session observe the identical ordered journal — the same (event-id, payload-hash)
// sequence — with a Last-Event-ID reconnect on one of them (spec §9, §21.1; SES-001). The
// session is a real engine-driven run, so the journal is genuine, not seeded.
func TestTwoClientsSeeIdenticalOrderedJournal(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	respID, sessionID, _ := h.admit()
	h.awaitResponseState(respID, "completed", 60*time.Second)

	// Client A reads the whole journal in one connection, through the terminal close.
	a := h.openAttach(sessionID, nil)
	defer a.close()
	seqA := a.drain(t)
	if len(seqA) < 4 {
		t.Fatalf("client A saw %d frames, want a multi-event journal to make the reconnect meaningful", len(seqA))
	}
	if last := seqA[len(seqA)-1]; last.typ != "run.completed.v1" {
		t.Fatalf("last frame type = %q, want run.completed.v1 (stream must drain through the terminal)", last.typ)
	}

	// Client B reads the first half, drops the connection, then reconnects from the last
	// confirmed event id and reads the rest — the union is B's full sequence.
	b1 := h.openAttach(sessionID, nil)
	half := len(seqA)/2 + 1
	firstHalf := make([]attachFrame, 0, half)
	for i := 0; i < half; i++ {
		f, ok := b1.next(t)
		if !ok {
			t.Fatalf("client B closed after %d frames before the split point %d", i, half)
		}
		firstHalf = append(firstHalf, f)
	}
	resumeID := firstHalf[len(firstHalf)-1].id
	b1.close()

	b2 := h.openAttach(sessionID, map[string]string{"Last-Event-ID": resumeID})
	defer b2.close()
	seqB := append(firstHalf, b2.drain(t)...)

	// The two clients' (id, payloadHash) sequences must be element-for-element identical.
	if len(seqA) != len(seqB) {
		t.Fatalf("client A saw %d frames, client B saw %d — the journals diverge", len(seqA), len(seqB))
	}
	for i := range seqA {
		if seqA[i].id != seqB[i].id || seqA[i].payloadHash != seqB[i].payloadHash {
			t.Fatalf("frame %d diverges: A=(%s,%s) B=(%s,%s)",
				i, seqA[i].id, seqA[i].payloadHash, seqB[i].id, seqB[i].payloadHash)
		}
	}
	// The agreed order is the canonical gap-free 1..N sequence.
	for i, f := range seqA {
		if f.seq != i+1 {
			t.Fatalf("frame %d sequence = %d, want %d (ordered journal)", i, f.seq, i+1)
		}
	}
}

// TestUnauthorizedAttachIsTenantScoped404WithAuditDenial proves a cross-tenant attach is a
// tenant-scoped 404 that discloses no existence, and appends a content-free denial to the
// append-only audit log keyed to the ACTOR's tenant (spec §39.2, §50.3; SES-002).
func TestUnauthorizedAttachIsTenantScoped404WithAuditDenial(t *testing.T) {
	h := newHarness(t)

	// Tenant A (the harness tenant) owns a real session.
	_, sessionA, _ := h.admitWith(`{"input":"a"}`, newID("idem"))

	// Tenant B is a separate tenant; its key must not reach A's session.
	otherToken := newID("e2e-tok")
	tenantB := seedTenantWithKey(t, h.spine.Pool(), otherToken)

	req, err := http.NewRequest(http.MethodGet, h.base+"/v1/sessions/"+sessionA+"/events", nil)
	if err != nil {
		t.Fatalf("build attach GET error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+otherToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("attach GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant attach status = %d, want 404", resp.StatusCode)
	}
	// A content-free not_found problem — never a signal the id exists in another tenant.
	var problem contracts.Problem
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem error = %v", err)
	}
	if problem.Code != "not_found" {
		t.Fatalf("problem code = %q, want not_found", problem.Code)
	}

	// The denial row is keyed to the actor (tenant B), names only the caller-supplied id, and
	// discloses nothing about tenant A.
	var org, project, actor, action, outcome, resource string
	var detail []byte
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT organization_id, project_id, actor, action, outcome, resource, detail
		   FROM audit_events WHERE resource=$1 AND outcome='denied' ORDER BY id DESC LIMIT 1`,
		sessionA).Scan(&org, &project, &actor, &action, &outcome, &resource, &detail); err != nil {
		t.Fatalf("read audit denial error = %v", err)
	}
	if org != tenantB.Organization || project != tenantB.Project {
		t.Fatalf("denial keyed to %s/%s, want the actor's tenant %s/%s", org, project, tenantB.Organization, tenantB.Project)
	}
	if action != "session.attach" || outcome != "denied" {
		t.Fatalf("denial action/outcome = %s/%s, want session.attach/denied", action, outcome)
	}
	// Content-free: nothing in the row identifies tenant A (the true owner).
	leak := string(detail) + org + project + actor
	if strings.Contains(leak, h.tenant.Organization) || strings.Contains(leak, h.tenant.Project) {
		t.Fatalf("denial discloses tenant A: detail=%s org=%s project=%s actor=%s", detail, org, project, actor)
	}
}

// TestProfileFreeSessionRunsAllCoreFeatures proves the profile-free path (agent_revision_id
// null — no AgentProfile/Revision exists until E11) runs this slice's core session features:
// a multi-step run, a mid-run steer delivered through the command spine, and a chained
// continuation on the same session. agent_revision_id is explicitly null on every create
// (spec §9; AGT-003, deterministic half — the live journey folds it in at Milestone 2).
func TestProfileFreeSessionRunsAllCoreFeatures(t *testing.T) {
	h := newHarness(t)
	gp := newGatedProvider()
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, gp))
	defer stop()

	// Response 1: a profile-free multi-step run.
	resp1, sessionID, _ := h.admitWith(`{"input":"first turn","agent_revision_id":null}`, newID("idem"))

	// Core feature — command spine: a steer delivered at the mid-run boundary.
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}
	commandID := newID("cmd")
	if cmd := h.submitCommand(sessionID, `{"command_id":"`+commandID+`","kind":"send_message","delivery":"steer","message":"STEER"}`); cmd.Status != "queued" {
		t.Fatalf("steer status = %q, want queued (a live profile-free run accepts it)", cmd.Status)
	}
	close(gp.release)
	h.awaitResponseState(resp1, "completed", 60*time.Second)

	// Core feature — chaining: response 2 continues the same session, also profile-free.
	resp2, session2, _ := h.admitWith(`{"input":"second turn","previous_response_id":"`+resp1+`","agent_revision_id":null}`, newID("idem"))
	if session2 != sessionID {
		t.Fatalf("chained response session = %q, want the same session %q", session2, sessionID)
	}
	h.awaitResponseState(resp2, "completed", 60*time.Second)

	// The steer applied and both responses live on one gap-free journal — all with no agent
	// revision bound.
	if state, appliedSeq := h.commandRow(commandID); state != "applied" || appliedSeq == nil {
		t.Fatalf("steer state = %q applied_sequence = %v, want applied", state, appliedSeq)
	}
	events := h.events(sessionID)
	assertContiguous(t, events)
	if len(events) < 4 {
		t.Fatalf("profile-free journal has %d events, want both responses plus the steer", len(events))
	}
}
