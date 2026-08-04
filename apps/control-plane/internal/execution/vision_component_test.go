//go:build component

package execution

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"

	"github.com/palgroup/palai/storage"
)

// recordingAdapter captures the Request the broker actually routed. It exists because the deterministic fake
// adapter IGNORES images — as a test double should — so asserting "the model saw it" against the fake would
// be asserting nothing. This one records and answers.
type recordingAdapter struct {
	mu   sync.Mutex
	seen []modelbroker.Request
	out  string
}

func (a *recordingAdapter) Execute(_ context.Context, req modelbroker.Request, _ string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	a.mu.Lock()
	a.seen = append(a.seen, req)
	a.mu.Unlock()
	if onDelta != nil {
		onDelta(modelbroker.Delta{Text: a.out})
	}
	return modelbroker.Result{
		ModelRequestID: req.ModelRequestID, ProviderRequestID: "rec_1", Model: req.Model,
		Output: a.out, FinishReason: "stop", Attempts: 1,
	}, nil
}

func (a *recordingAdapter) requests() []modelbroker.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]modelbroker.Request(nil), a.seen...)
}

// stubImageReader is the object store's read side. The REAL one is exercised against real S3 in
// apps/control-plane/internal/artifacts/inbound_image_component_test.go; what this stands in for here is only
// the byte source, so this test can be about the DISPATCH seam.
type stubImageReader struct {
	mu    sync.Mutex
	table map[string]struct {
		mediaType string
		content   []byte
	}
	scopes []string // every (org, project, id) the resolver asked for, so the tenant binding is assertable
}

func (r *stubImageReader) ReadImageArtifact(_ context.Context, project, id string) (string, []byte, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopes = append(r.scopes, org+"/"+project+"/"+id)
	got, ok := r.table[id]
	if !ok {
		return "", nil, false, nil
	}
	return got.mediaType, got.content, true, nil
}

// TestDispatchResolvesAnImageRefIntoTheProviderRequest is the SPINE proof, and it closes the one gap every
// other test in this change leaves open: the pieces are each proven, but the wiring between them is not.
//
// It drives the real dispatchModel over real PostgreSQL with an engine frame whose conversation carries an
// `image_ref` content item — exactly what the engine sends when a Slack-born run's input named an artifact —
// and asserts the BYTES arrived in the provider request. If SetImageReader were never called, or the resolver
// never consulted, or decodeMessages serialised the array as it used to, this fails.
//
// It also asserts the read was scoped to the RUN'S OWN TENANT. The artifact id comes out of the run's input,
// which is untrusted content, so the id must be a lookup key inside a tenant the run already fixed — never a
// selector for one.
func TestDispatchResolvesAnImageRefIntoTheProviderRequest(t *testing.T) {
	cs, tenant, exec := openPinnedSpine(t)
	ctx := context.Background()
	sessionID, responseID, runID := pinnedID("ses"), pinnedID("resp"), pinnedID("run")
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Project)
	exec(`INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'queued')`,
		responseID, tenant.Project, sessionID)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'running')`,
		runID, tenant.Project, sessionID, responseID)

	const artifactID = "art_component_vision"
	images := &stubImageReader{table: map[string]struct {
		mediaType string
		content   []byte
	}{artifactID: {mediaType: "image/png", content: []byte("PNGSCREENSHOT")}}}

	adapter := &recordingAdapter{out: "the image says PALAI"}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"rec": adapter},
		Secrets:  modelbroker.StaticResolver{"rec": "unused"},
	})
	ch := &recordingChannel{}
	orch := &Orchestrator{spine: cs, models: broker, route: ModelRoute{Provider: "rec", Model: "rec-model", Secret: "rec"}}
	orch.SetImageReader(images) // the production wiring, through the production setter
	st := &attemptState{
		attempt:    AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att")), Fence: 1},
		tenant:     tenant,
		sessionID:  sessionID,
		responseID: responseID,
		ch:         ch,
	}

	requestID := pinnedID("mreq")
	// The frame the engine actually sends: Context.start appends {"role":"user","content": <the run's input>},
	// and a Slack-born run's input with an image IS this array.
	frame := contracts.EngineFrame{Type: "model.request", Data: map[string]any{
		"model_request_id": requestID,
		"messages": []any{
			map[string]any{"role": "system", "content": "kernel instructions"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "ne yazıyor burada"},
				map[string]any{"type": "image_ref", "artifact_id": artifactID},
			}},
		},
	}}

	if _, err := orch.dispatchModel(ctx, st, frame); err != nil {
		t.Fatalf("dispatchModel: %v", err)
	}

	routed := adapter.requests()
	if len(routed) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(routed))
	}
	// The system turn is untouched, and the user turn carries the words plus the BYTES.
	msgs := routed[0].Messages
	if len(msgs) != 2 {
		t.Fatalf("routed messages = %d (%+v), want 2", len(msgs), msgs)
	}
	if msgs[0].Content != "kernel instructions" || len(msgs[0].Images) != 0 {
		t.Fatalf("system turn = %+v, want it unchanged and image-free", msgs[0])
	}
	if msgs[1].Content != "ne yazıyor burada" {
		t.Fatalf("user content = %q, want the human's words — NOT the serialised array the old path produced", msgs[1].Content)
	}
	if len(msgs[1].Images) != 1 {
		t.Fatalf("user images = %d, want the resolved image", len(msgs[1].Images))
	}
	if got := msgs[1].Images[0]; got.MediaType != "image/png" || string(got.Data) != "PNGSCREENSHOT" {
		t.Fatalf("routed image = %+v, want the resolved png bytes", got)
	}
	// The artifact id must never appear in what the model reads: it is internal scope.
	if strings.Contains(msgs[1].Content, artifactID) {
		t.Fatalf("user content %q carries the artifact id", msgs[1].Content)
	}

	// THE TENANT BINDING: the read was scoped to the run's own org/project, taken from the attempt and not
	// from anything in the content item.
	images.mu.Lock()
	scopes := append([]string(nil), images.scopes...)
	images.mu.Unlock()
	want := tenant.Organization + "/" + tenant.Project + "/" + artifactID
	if len(scopes) != 1 || scopes[0] != want {
		t.Fatalf("resolver was asked for %v, want exactly [%s]", scopes, want)
	}

	// And the step really committed: this is a full dispatch, not a half-run.
	var state string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT state FROM model_requests WHERE id=$1`, requestID).Scan(&state); err != nil {
		t.Fatalf("read model_request state: %v", err)
	}
	if state != "completed" {
		t.Fatalf("model_request state = %q, want completed", state)
	}
}

// TestDispatchWithNoImageReaderMarksTheImageRatherThanFailing proves the nil-safe posture every optional seam
// in this orchestrator takes: a deployment with no object store wired resolves every image_ref as a miss, the
// turn SAYS the image is unavailable, and the run continues. Silently answering as though nothing was
// attached is the failure this whole change exists to remove; failing the run instead would take a stack with
// no object store from "cannot show images" to "cannot answer".
func TestDispatchWithNoImageReaderMarksTheImageRatherThanFailing(t *testing.T) {
	cs, tenant, exec := openPinnedSpine(t)
	ctx := context.Background()
	sessionID, responseID, runID := pinnedID("ses"), pinnedID("resp"), pinnedID("run")
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Project)
	exec(`INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'queued')`,
		responseID, tenant.Project, sessionID)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'running')`,
		runID, tenant.Project, sessionID, responseID)

	adapter := &recordingAdapter{out: "I cannot see an image"}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"rec": adapter},
		Secrets:  modelbroker.StaticResolver{"rec": "unused"},
	})
	// NO SetImageReader — the deployment has no object store.
	orch := &Orchestrator{spine: cs, models: broker, route: ModelRoute{Provider: "rec", Model: "rec-model", Secret: "rec"}}
	st := &attemptState{
		attempt:    AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att")), Fence: 1},
		tenant:     tenant,
		sessionID:  sessionID,
		responseID: responseID,
		ch:         &recordingChannel{},
	}
	frame := contracts.EngineFrame{Type: "model.request", Data: map[string]any{
		"model_request_id": pinnedID("mreq"),
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "bak"},
			map[string]any{"type": "image_ref", "artifact_id": "art_nowhere"},
		}}},
	}}

	if _, err := orch.dispatchModel(ctx, st, frame); err != nil {
		t.Fatalf("dispatchModel: %v", err)
	}
	routed := adapter.requests()
	if len(routed) != 1 || len(routed[0].Messages) != 1 {
		t.Fatalf("routed = %+v, want one message", routed)
	}
	msg := routed[0].Messages[0]
	if len(msg.Images) != 0 {
		t.Fatalf("images = %+v, want none with no reader wired", msg.Images)
	}
	if !strings.Contains(msg.Content, "bak") {
		t.Fatalf("content = %q, want the human's words kept", msg.Content)
	}
	if !strings.Contains(msg.Content, "no longer available") {
		t.Fatalf("content = %q, want it to SAY the image is unavailable — a silent drop is the defect this change removes", msg.Content)
	}
}
