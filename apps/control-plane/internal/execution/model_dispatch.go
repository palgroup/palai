package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/metrics"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// interruptPollInterval is how often the in-flight-abort watcher checks for a pending interrupt
// while a provider call is outstanding (spec §9.2, §25.11). ponytail: a DB poll during the
// model call; a LISTEN/NOTIFY signal would drop the poll if model-call latency ever makes it
// matter. The watcher only runs for the duration of one provider call.
const interruptPollInterval = 25 * time.Millisecond

// errProviderRejected marks a provider-side rejection (a non-nil Result.Error) for the metrics
// counter, when the transport returned no Go error. It is never surfaced to a caller — the run
// failure is built from result.Error's sanitized fields below.
var errProviderRejected = errors.New("provider rejected the request")

// providerCallOutcome classifies a broker.Route outcome for palai_provider_errors_total: it returns a
// non-nil error ONLY for an UPSTREAM failure — an adapter/transport error, or a provider-side rejection
// carried on the result. The provider call still HAPPENED (counts toward calls_total) but did not fail
// upstream when the error is a config problem (unknown provider/secret), the platform's own budget
// cutoff (ErrBudgetExceeded), or a user interrupt cancellation — so those return nil here and never
// trip the provider-error alert with the upstream healthy.
func providerCallOutcome(routeErr error, result modelbroker.Result) error {
	switch {
	case routeErr == nil:
		if result.Error != nil {
			return errProviderRejected
		}
		return nil
	case errors.Is(routeErr, context.Canceled),
		errors.Is(routeErr, modelbroker.ErrUnknownProvider),
		errors.Is(routeErr, modelbroker.ErrUnknownSecret),
		errors.Is(routeErr, modelbroker.ErrBudgetExceeded):
		return nil
	default:
		return routeErr // an adapter.Execute transport/upstream error
	}
}

// interruptHit is the watcher's verdict for one model call: found reports whether a pending
// interrupt was seen (and canceled the call). kind branches the handler — a send_message folds
// its message, a change_config applies its revision + warns (spec §9.2, §9.3). payload carries
// the raw command content the handler decodes.
type interruptHit struct {
	found     bool
	commandID string
	kind      string
	message   string
	payload   []byte
}

// ModelRoute is the broker coordinates this kernel routes a model.request to: the
// adapter name, the model id put on the provider wire, and the SecretRef the executor
// redeems at call time.
//
// It has two provenances. As the Orchestrator's `route` field it is the DEPLOYMENT DEFAULT the
// composition root selects from the environment (PALAI_MODEL_PROVIDER/PALAI_MODEL), defaulting to the
// deterministic fake provider every existing suite registers under. As the value effectiveRoute returns
// it may instead be the project's DB-backed route (E13 T8) — in which case RevisionID/Revision name the
// model_route_revision that selected it and Secret is a tenant-qualified handle.
type ModelRoute struct {
	Provider string
	Model    string
	Secret   modelbroker.SecretRef
	// BaseURL is the CONNECTION's endpoint (E29, migration 000049), empty meaning the family's own. It is
	// the seam that makes a custom OpenAI-compatible provider a PER-PROJECT property: before it the only
	// way to move that endpoint was PALAI_OPENAI_COMPATIBLE_BASE_URL, read once at boot into a single
	// adapter value, so one deployment had exactly one custom endpoint no matter how many projects it ran.
	BaseURL string
	// RevisionID / Revision are set ONLY when the route was resolved from the project's DB route. An
	// empty RevisionID means this is the deployment default, which pins no revision.
	RevisionID string
	Revision   int
}

var defaultModelRoute = ModelRoute{Provider: "fake", Model: "fake", Secret: modelbroker.SecretRef("model")}

// dispatchModel handles a model.request. A committed result for the stable
// model_request_id is replayed without re-calling the provider (the DB half of
// cross-attempt dedup, spec §53.4). Otherwise the request is persisted (row + event)
// BEFORE the provider is called (spec §24.7 order), the result is committed, and only
// then is model.result delivered (commit-before-deliver). Provider tool calls cross
// the engine boundary as objects, never the raw JSON string (spec §25.9).
//
// It reports whether the result carries tool calls: a result with tool calls means the run
// continues to another model step, so the command pump has a next input boundary to deliver a
// queued/steered message into. A final result (no tool calls) is the run's last step.
func (o *Orchestrator) dispatchModel(ctx context.Context, st *attemptState, frame contracts.EngineFrame) (bool, error) {
	requestID, _ := frame.Data["model_request_id"].(string)
	// Record the boundary a tool call produced by THIS step's model.result belongs to, so a side-effecting
	// tool's durable pre-write records commit_boundary = this model_request_id (E12 T4). One assign; both
	// the replay and the live branch route through here.
	st.lastModelRequestID = requestID

	if stored, found, err := o.spine.LookupModelResult(ctx, st.tenant, requestID); err != nil {
		return false, err
	} else if found {
		var data map[string]any
		if err := json.Unmarshal(stored, &data); err != nil {
			return false, fmt.Errorf("replay model result %s: %w", requestID, err)
		}
		// The committed result carries the used model, so the replay branch fills the
		// terminal projection's model without re-routing (spec §53.4).
		if model, ok := data["model"].(string); ok {
			st.model = model
		}
		toolCalls, _ := data["tool_calls"].([]any)
		// Nothing to re-account here: a stored result was settled into the usage ledger by the SAME
		// transaction that stored it (E13 T6), so replaying it must NOT settle again — and cannot,
		// since the ledger key is derived from this model_request_id.
		return len(toolCalls) > 0, st.ch.Send(ctx, o.frame(st, "model.result", data, string(frame.ID)))
	}

	messages, err := decodeMessages(frame.Data["messages"], o.imageResolver(ctx, st))
	if err != nil {
		return false, fmt.Errorf("model request %s: %w", requestID, err)
	}

	// Resolve the run's route: the project's DB-backed model route when it has one, else the deployment
	// default (E13 T8, spec §27.6). Both the adapter+credential the call is made with and the model id's
	// base layer come from here.
	route, err := o.effectiveRoute(ctx, st)
	if err != nil {
		return false, err
	}

	// Resolve the per-step effective model from the session's config revisions (spec §9.3,
	// §25.16). A normal switch's revision applied at the previous boundary, and an immediate
	// switch's applied at the interrupt boundary, so this step routes under the current config.
	effectiveModel, err := o.effectiveModel(ctx, st)
	if err != nil {
		return false, err
	}

	// Advertise the run's effective tool set to the provider (spec §14, §28.5, E12 T1). Resolved per
	// step — the SAME layering effectiveModel/effectiveConfigHash use — so a config change at the
	// previous boundary is reflected here for free. An empty effective set leaves Tools nil, so a run
	// that configures no tools routes a provider request that is bit-for-bit the pre-advertising one.
	// The run's pinned revision, read ONCE for this step and used by both readers below: the tool
	// resolution, and the instruction stack. It was read inside advertisedTools before; it is hoisted
	// here because a second reader arrived and two independent reads of the same pin are how a step's
	// advertised tools and its instructions come to describe different revisions.
	pinned, err := o.spine.PinnedExecConfig(ctx, st.tenant, string(st.attempt.RunID))
	if err != nil {
		return false, err
	}
	advertised, skills, err := o.advertisedTools(ctx, st, pinned)
	if err != nil {
		return false, err
	}
	// Progressive loading (spec §28.16): when the run pinned skills, prepend ONE system message carrying
	// their metadata (name/description/path) ahead of the engine-assembled conversation — the body is
	// NEVER inlined; the model reads it on-demand via the file tool. A skill-less run appends nothing, so
	// its provider request stays BIT-IDENTICAL to the pre-skills path (T1 advertising regression).
	if len(skills) > 0 {
		messages = append([]modelbroker.Message{skillContextMessage(skills)}, messages...)
	}
	// The run's §25.12 instruction stack: the pinned revision's instructions (layer 3), then the
	// request's own (layer 5), placed after the engine's kernel turns and above the conversation.
	// THIS IS THE LINE THAT WAS MISSING. Both sources were durable — agent_revisions.instructions
	// since E11, and now runs.instructions — and neither reached the conversation the provider is
	// called with, which the engine assembles and which is never told about either. Measured against
	// a real provider on 2026-08-01: a run pinning a revision with 320 words of instructions and a run
	// pinning nothing both produced usage.input_tokens 48, and the pinned "answer in one word" was not
	// obeyed. A run with neither layer gets `messages` back unchanged.
	messages = applyInstructionLayers(messages, resolveInstructionLayers(pinned.Instructions, st.runInstructions))

	// before_model hooks fire AFTER the effective tool set is resolved, BEFORE the request is committed or
	// the provider is called (spec §28.17, the pin). A policy DENY here blocks the model step VISIBLY: it
	// journals policy.denied.v1 and fails the attempt with the reason — no model.request row, no provider
	// call. There is no result to deliver at this point (the model step itself is refused), so unlike
	// before_tool this is a hard step failure, not a folded denial. No-op when no firer is wired.
	if outcome, err := o.fireHook(ctx, st, extensions.HookPointBeforeModel, map[string]any{"tool_count": len(advertised), "model": effectiveModel}); err != nil {
		return false, err
	} else if outcome.Denied {
		if err := o.journalPolicyDenied(ctx, st, extensions.HookPointBeforeModel, outcome.HookID, outcome.Reason,
			map[string]any{"model_request_id": requestID}); err != nil {
			return false, err
		}
		return false, fmt.Errorf("before_model policy hook %s denied the model step: %s", outcome.HookID, outcome.Reason)
	}

	requestEvent, _ := json.Marshal(map[string]any{"run_id": st.attempt.RunID, "model_request_id": requestID})
	if err := o.spine.CommitModelRequest(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), requestID, eventModelStepCreated, requestEvent); err != nil {
		return false, err
	}

	// Watch for an interrupt while the provider call is outstanding: an interrupt command
	// cancels this call's context (the §25.11 in-flight-abort controller half). The watcher
	// runs only for this call and always reports exactly once on modelCtx being canceled.
	modelCtx, cancelModel := context.WithCancel(ctx)
	defer cancelModel()
	hitCh := make(chan interruptHit, 1)
	go o.watchInterrupt(modelCtx, st, cancelModel, hitCh)

	// Accumulate the streamed-so-far text so an interrupt can record it as the explicit partial
	// item (spec §25.16), not None. onDelta runs synchronously inside Route on this goroutine,
	// so a plain builder needs no lock.
	//
	// THE SECOND READER of that same callback is the delta sink, and it is the half that was missing.
	// The accumulated text used to be observable ONLY when the step was interrupted, so a step that
	// completed normally streamed its answer to nobody and the client saw created -> completed with
	// everything arriving at once. The sink coalesces the same text into windows and journals each one
	// as an advisory model_step.delta.v1. It cannot write inline here: onDelta runs inside the
	// provider's read loop, and a Postgres round trip there stalls the stream it reports on
	// (model_delta_sink.go).
	//
	// It is given the OUTER ctx rather than modelCtx on purpose — modelCtx is cancelled to abort the
	// provider call on interrupt, and the tail of an interrupted step is exactly the text worth having.
	var partial strings.Builder
	deltas := newDeltaSink(ctx, func(dctx context.Context, text string) {
		// Best-effort by contract: a delta describes text whose authoritative record is the step's own
		// terminal event, so a failed append must not fail the step. The error is dropped here rather
		// than returned to a caller with nothing useful to do with it.
		_ = o.spine.AppendModelStepDelta(dctx, st.tenant, st.sessionID, st.responseID,
			string(st.attempt.RunID), requestID, text)
	})
	onDelta := func(d modelbroker.Delta) {
		if d.Text != "" {
			partial.WriteString(d.Text)
			deltas.add(d.Text)
		}
	}

	result, err := o.models.Route(modelCtx, route.Provider, modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID(requestID),
		// Stable across attempts: the same run and model-request identity re-derive the
		// same key, so a reclaimed attempt that re-routes carries it and the provider
		// settles one effect (spec §53.4, §35.3).
		IdempotencyKey: string(st.attempt.RunID) + "/" + requestID,
		Model:          effectiveModel,
		Messages:       messages,
		// The effective tool set (nil when nothing is configured). ForceToolCall stays false in
		// production: the model chooses whether to call — the forced (tool_choice:required) pattern
		// is a test seam only (E09 T4, tools/live), never set here.
		Tools: advertised,
		// The run's §22.7 output contract, rendered as the provider's own decoding constraint. nil
		// for every run that named no format, which keeps a text run's provider request BIT-IDENTICAL
		// to the pre-§22.7 one (the T1 advertising-regression discipline). This is the line that was
		// missing: the field, the adapters that send it and the endpoint check that refuses it all
		// shipped long ago, and nothing ever assigned it.
		OutputSchema: brokerOutputSchema(st.outputContract),
		// A ChildRun runs under its parent-intersected budget (spec §25.18); a root run stays
		// unbounded here (0). Enforcement is the broker's Reservation.Admit at settle.
		Reservation: modelbroker.Reservation{MaxTotalTokens: st.childBudget},
		// The credential the broker redeems at call time. A DB-routed project's ref is tenant-qualified,
		// so the redemption is scoped to that project's own organization (E13 T8). Zero on the deployment
		// default, which pins no revision.
		Secret: route.Secret,
		// The endpoint the CONNECTION named. Empty on the deployment default and on every OpenAI/Anthropic
		// connection, in which case each adapter dials exactly where it did before this field existed.
		BaseURL:       route.BaseURL,
		RouteRevision: route.Revision,
	}, onDelta)
	cancelModel()
	// Flush the last window BEFORE this step's terminal event is journalled. close() is synchronous for
	// that reason: a delta that landed after its own model_step.completed.v1 would put a client's
	// transcript out of order, which is a worse failure than a delta that never arrived at all.
	deltas.close()
	// Provider call/error counters (E14 Task 6): count the call, count an error only for an UPSTREAM
	// failure (see providerCallOutcome — config/budget/interrupt do not count).
	metrics.RecordProviderCall(providerCallOutcome(err, result))
	hit := <-hitCh
	// An interrupt that actually aborted the in-flight call (canceled ctx) ends this step
	// partial and resumes in a new one, folding a message or applying the new config (spec §9.2,
	// §9.3). An interrupt that raced a normal return leaves the command queued for a boundary.
	if hit.found && errors.Is(err, context.Canceled) {
		return false, o.handleInterrupt(ctx, st, frame, requestID, hit, partial.String())
	}
	if err != nil {
		return false, fmt.Errorf("route model request %s: %w", requestID, err)
	}
	// A provider-side failure rides on the RESULT, not the Go error (the adapter contract:
	// modelbroker.Result.Error, sanitized of any credential). Until E13 T8 nothing read it, so a 401 or a
	// 429 was committed as a successful step with empty output — a silent wrong answer. It must fail the
	// step: with per-project credentials a rejected credential is an ordinary tenant-caused condition, and
	// the tenant has to see it. The sanitized message carries no credential, so it is safe to surface.
	if result.Error != nil {
		failure := fmt.Errorf("model request %s: provider_error: %s (code %s, status %d)",
			requestID, result.Error.Message, result.Error.Code, result.Error.Status)
		// An oversized request is named rather than left to the retry ladder (E21 T1): compaction
		// makes it rare, not impossible — the budget is a byte estimate, not the provider's tokenizer.
		if isContextOverflow(result.Error) {
			return false, fmt.Errorf("%w: %w", errContextOverflow, failure)
		}
		// A permanent rejection is NAMED too, for the same reason and by the same mechanism: the retry
		// ladder cannot change a request the provider has already refused (orchestrator's
		// isPermanentProviderRejection).
		if isPermanentProviderRejection(result.Error) {
			return false, fmt.Errorf("%w: %w", errProviderPermanent, failure)
		}
		return false, failure
	}
	st.usage = addUsage(st.usage, result.Usage)
	st.model = result.Model

	toolCalls, err := toEngineToolCalls(result.ToolCalls)
	if err != nil {
		// A tool-call arguments string that is not a JSON object is a provider fault,
		// sanitized here; the raw string never reaches the engine (spec §25.9).
		return false, fmt.Errorf("model request %s: provider_error: %w", requestID, err)
	}

	data := map[string]any{"model_request_id": requestID, "model": result.Model}
	// Persist the provider's own request id (a chatcmpl-... for provider-one, the fake id
	// for the deterministic adapter). It is safe, non-secret correlation evidence — the UAT
	// reads it back from the committed result for the live-round-trip receipt.
	if result.ProviderRequestID != "" {
		data["provider_request_id"] = result.ProviderRequestID
	}
	// "Every model step records the actual selected target" (spec §27.6). Absent on the deployment
	// default, so a stack with no DB route commits a bit-identical result.
	if route.RevisionID != "" {
		data["route_revision_id"] = route.RevisionID
	}
	if result.Output != "" {
		data["output"] = result.Output
	}
	if len(toolCalls) > 0 {
		data["tool_calls"] = toolCalls
	}

	stored, _ := json.Marshal(data)
	resultEvent, _ := json.Marshal(map[string]any{"run_id": st.attempt.RunID, "model_request_id": requestID})
	// result.Usage rides the commit so the step's provider usage is settled into the ledger in the same
	// transaction as the result itself (spec §43.1, E13 T6). It is deliberately NOT folded into `data`:
	// that map is also the model.result frame the engine receives, and usage is control-plane metering,
	// not engine input.
	if _, err := o.spine.CommitModelResult(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), requestID, stored, eventModelStepCompleted, resultEvent, result.Usage); err != nil {
		return false, err
	}
	return len(toolCalls) > 0, st.ch.Send(ctx, o.frame(st, "model.result", data, string(frame.ID)))
}

// decodeMessages converts the engine's assembled conversation into canonical model
// messages. The engine carries tool-call arguments and content as JSON objects, while
// the canonical message shape carries both as strings, so this is the inbound half of
// the same string/object boundary toEngineToolCalls owns outbound (spec §25.9).
//
// A turn whose content is a CONTENT ARRAY (the §25.10 richer shape, content.json's typed items) is folded
// into text + images here rather than serialised: `resolve` turns each image_ref's artifact id into bytes,
// control-plane-side. That resolution CANNOT live in the engine — the object-store credential is
// control-plane-only (spec §24) and the 1 MiB engine frame ceiling (execute_run.go MaxFrameBytes) could not
// carry a screenshot anyway, which is why the input carries a REFERENCE and the bytes join here.
func decodeMessages(raw any, resolve imageResolver) ([]modelbroker.Message, error) {
	items, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("messages is not an array")
	}
	items = flattenWrappedMessages(items)
	out := make([]modelbroker.Message, 0, len(items))
	// The image budget is spent on the MOST RECENT images, so `budget` starts as the number of images this
	// conversation must skip before it starts resolving. See countImageRefs for why it works this way round.
	budget := countImageRefs(items) - maxRunImages
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		msg := modelbroker.Message{}
		if parts, ok := fields["content"].([]any); ok {
			text, resolved, err := decodeContentParts(parts, resolve, &budget)
			if err != nil {
				return nil, err
			}
			msg.Content, msg.Images = text, resolved
		} else {
			msg.Content = asJSONString(fields["content"])
		}
		msg.Role, _ = fields["role"].(string)
		msg.ToolCallID, _ = fields["tool_call_id"].(string)
		if calls, ok := fields["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				id, _ := call["id"].(string)
				name, _ := call["name"].(string)
				msg.ToolCalls = append(msg.ToolCalls, modelbroker.ToolCall{ID: id, Name: name, Arguments: asJSONString(call["arguments"])})
			}
		}
		out = append(out, msg)
	}
	return out, nil
}

// flattenWrappedMessages undoes an engine wrap that turns a whole CONVERSATION into one turn's content,
// and without it every request made in the public API's message-array form silently loses all of its
// content — text and images alike.
//
// WHAT THE ENGINE DOES. engines/reference/src/palai_engine/context.py's start() appends the run's input
// as one user turn, whatever shape it has:
//
//	self._messages.append({"role": "user", "content": run_start_data["input"]})
//
// That is right for the two shapes it was written for — a bare string, and an array of CONTENT ITEMS
// (which is what the Slack bridge sends). It is wrong for the third shape the Responses API accepts and
// the SDKs encourage, an array of MESSAGES, because the result is a user turn whose content array holds
// message objects rather than content items.
//
// WHAT THAT COST, measured 2026-08-04 on the live deployment. decodeContentParts keys on an item's
// `type`; a message object has none, so every item matched no case and was dropped. The turn arrived at
// the provider with no text and no image, the adapter rendered its one empty text block, and Anthropic
// answered 400 invalid_request_error once per second forever. Every response whose input used this shape
// failed; every response that sent a bare string succeeded, 34 seconds apart, on the same route and
// model. The image leg looked broken and was not — the content never reached it.
//
// WHY THE FIX IS HERE AND NOT IN THE ENGINE. The engine is a pinned container image and its wrap is
// legitimate for the shapes it handles; this decoder is the contract boundary that already normalises
// what the engine emits into modelbroker.Message, and it is the half that can be proved from Go. The
// engine line above is the upstream writer and is worth fixing there too — recorded rather than silently
// worked around.
//
// The discriminator is exact rather than heuristic: a content item always carries `type` and never
// `role`, and a message always carries `role` and `content`. A single non-conforming element leaves the
// turn untouched, so a genuine content array is never re-interpreted.
func flattenWrappedMessages(items []any) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		parts, ok := fields["content"].([]any)
		if !ok || len(parts) == 0 || !allMessageShaped(parts) {
			out = append(out, item)
			continue
		}
		out = append(out, parts...)
	}
	return out
}

// allMessageShaped reports whether every element is a MESSAGE object rather than a content item.
func allMessageShaped(parts []any) bool {
	for _, part := range parts {
		fields, ok := part.(map[string]any)
		if !ok {
			return false
		}
		if _, isContentItem := fields["type"].(string); isContentItem {
			return false
		}
		if role, ok := fields["role"].(string); !ok || role == "" {
			return false
		}
		if _, hasContent := fields["content"]; !hasContent {
			return false
		}
	}
	return true
}

// ImageReader is the object-store read path an image content item resolves through. It is a separate
// one-method seam from ArtifactWriter on purpose: the write side has its own implementors (including test
// doubles), and widening it would have made every one of them carry a read they do not do.
type ImageReader interface {
	ReadImageArtifact(ctx context.Context, project, artifactID string) (mediaType string, content []byte, found bool, err error)
}

// imageResolver binds this run's TENANT to the object-store read, and that binding is the whole access
// control: the artifact id comes out of the run's own input, which is untrusted content, and the read is
// scoped to the run's organization+project so a foreign id reads no row at all (the §22.6 non-disclosure
// rule the retrieval API already relies on). An id cannot select a tenant — the tenant is already fixed by
// the run.
//
// A NON-IMAGE artifact is refused as a miss rather than sent: media types are recorded at write time, and a
// caller pointing an image_ref at a text artifact would otherwise have a diff base64'd into a vision request.
func (o *Orchestrator) imageResolver(ctx context.Context, st *attemptState) imageResolver {
	return func(artifactID string) (modelbroker.Image, bool, error) {
		if o.images == nil {
			return modelbroker.Image{}, false, nil // no object store wired: honestly unavailable
		}
		mediaType, content, found, err := o.images.ReadImageArtifact(ctx, st.tenant.Project, artifactID)
		if err != nil || !found {
			return modelbroker.Image{}, false, err
		}
		if !strings.HasPrefix(mediaType, "image/") {
			return modelbroker.Image{}, false, nil
		}
		return modelbroker.Image{MediaType: mediaType, Data: content}, true, nil
	}
}

// imageResolver turns an artifact id into the image's bytes and SNIFFED media type, or reports it
// unresolvable. found=false is a blameless miss (retention reaped it, or the row is not there); an error is
// a real storage failure. It is a func rather than an interface because there is exactly one production
// implementation and the tests want a literal (the one-implementation-interface rule).
type imageResolver func(artifactID string) (modelbroker.Image, bool, error)

// maxRunImages caps how many images ONE model request may carry, counted across the whole assembled
// conversation. The number is ours — no provider publishes a hard limit — and it exists because an image is
// the most expensive input a third party can put into a run: one 1080x144 screenshot MEASURED 8524 input
// tokens on gpt-4o-mini and a larger one 19858 (tests/live/provider/vision_live_test.go). Without a ceiling
// a workspace member prices a run arbitrarily by attaching files, and a long thread re-sends every image it
// ever accumulated on every single turn.
const maxRunImages = 8

// maxImageBytes caps ONE image's decoded size. A screenshot is far under it; a multi-hundred-megabyte
// "image" is a memory attack, and the bytes are buffered whole (base64 into a request body) so the bound has
// to be a real one. Enforced HERE as well as at every ingest point, because this is the last boundary before
// the bytes are copied into a provider request — an ingest-only bound would be one forgotten caller away
// from useless.
const maxImageBytes = 5 << 20 // 5 MiB

// NOTHING HERE FAILS THE STEP, and that is a correction to how this was first written rather than an
// oversight. The first version refused the model step when the conversation carried more images than the cap.
// Trace that through a real Slack thread: three images in message one, three in message two, three in message
// three — and the ninth trips the cap. But the conversation is REPLAYED on every turn, so from then on EVERY
// message in that thread fails, forever, including messages carrying no image at all. A cost ceiling would
// have become a thread-killer.
//
// So an image the request cannot carry becomes a MARKER in the conversation instead. That is still a visible
// refusal — arguably a better one: the model reads it and can tell the user which images it could not see,
// where a failed run tells them only that something broke. The rule "make the refusal visible, not silent" is
// satisfied by the marker, not by the failure.
const (
	// missingImageNote stands in for an image that could not be resolved — retention reaped it, or the row is
	// not there. It names no artifact id: the id is internal scope, and a prompt is not where scope belongs
	// (the slackRunInput rule).
	missingImageNote = "(an image shared in this conversation is no longer available)"
	// droppedImageNote stands in for an image dropped to stay under the per-request cap. The budget is spent
	// on the MOST RECENT images, so this only ever marks older ones — which is the right way round: the
	// question being asked now is about what was just shared.
	droppedImageNote = "(an older image from this conversation is not shown here, to stay within the images one request may carry)"
	// oversizeImageNote stands in for an image past the byte ceiling. Unreachable from the Slack path, which
	// bounds the fetch at the same ceiling; a hand-written /v1/responses body can still get here.
	oversizeImageNote = "(an image in this conversation is too large to show)"
	// redactedContentNote is what a purged prior turn says. It replaces what this path used to do with the
	// §22.2 redaction marker — serialise `[{"type":"redacted_content"}]` and present the JSON to the model as
	// the assistant's own prior words.
	redactedContentNote = "(the content of this turn was redacted by data retention)"
)

// countImageRefs totals the image_ref items across the whole assembled conversation.
//
// It exists so the budget can be spent on the NEWEST images rather than the first ones the walk happens to
// meet. decodeContentParts walks forward, so "keep the last N" is expressed as "skip the first total-N" —
// which needs the total up front. The alternative, walking backwards and reversing, buys nothing.
func countImageRefs(items []any) int {
	n := 0
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := fields["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			if p, ok := part.(map[string]any); ok && contracts.ContentItem(p).Type() == "image_ref" {
				n++
			}
		}
	}
	return n
}

// decodeContentParts folds ONE turn's content array into the provider-facing text plus its resolved images
// (spec §25.10's richer shape over content.json's typed items).
//
// WHAT IS TAKEN FROM AN ITEM, exhaustively, because an image is untrusted third-party input and this is the
// boundary that decides what of it becomes conversation:
//
//   - input_text / output_text / text → the `text` string, joined in order.
//   - image_ref → the `artifact_id`, used ONLY as a lookup key against the run's own tenant, and only the
//     resolved bytes + sniffed media type come back.
//   - redacted_content → the retention marker.
//   - anything else → SKIPPED. ContentItem is an open union (ADR-0002), so an unknown type must survive
//     rather than fail the step.
//
// Nothing else is read. An item's own `role`, `tools`, `organization_id`, `filename` or `text` cannot
// re-role the turn, grant a tool, select a tenant or contribute words — the item is data, never authority
// (the E17 T10 relay rule and the A2A untrusted-remote-result rule, applied to pixels).
//
// PROMPT INJECTION THROUGH THE IMAGE ITSELF IS NOT SOLVED HERE, and a reader meeting this function should
// know that rather than infer safety from the care above: text rendered INSIDE a screenshot reaches the
// model as model-visible content, and a model that reads "ignore your instructions" in a picture may act on
// it exactly as it may when it reads the same words in a message. What this boundary guarantees is narrower
// and worth having: an image cannot change the run's authority. Bounding what a model may DO with what it
// reads is the tool-surface and approval machinery's job (§28, the hooks), not this decode's.
//
// skip is the number of image_ref items still to be passed over before the budget starts being spent, so the
// cap keeps the NEWEST images rather than the first ones met (see countImageRefs). It is decremented, so it
// carries across turns.
func decodeContentParts(parts []any, resolve imageResolver, skip *int) (string, []modelbroker.Image, error) {
	var text []string
	var resolved []modelbroker.Image
	for _, part := range parts {
		item, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch contracts.ContentItem(item).Type() {
		case "input_text", "output_text", "text":
			// "text" is not in content.json; it is accepted because it is the name the OpenAI Chat shape
			// uses for the same thing, and a client that sends it would otherwise have its words silently
			// dropped. Only ever read on an item that DECLARES one of these types.
			if s, ok := item["text"].(string); ok && s != "" {
				text = append(text, s)
			}
		case "redacted_content":
			text = append(text, redactedContentNote)
		case "image_ref":
			id, _ := item["artifact_id"].(string)
			if id == "" {
				continue
			}
			if *skip > 0 {
				// Over the per-request cap: marked, not resolved. No read is issued for it either, so a
				// long thread does not pay a storage round trip per image it will not send.
				*skip--
				text = append(text, droppedImageNote)
				continue
			}
			img, found, err := resolve(id)
			if err != nil {
				return "", nil, err
			}
			switch {
			case !found:
				text = append(text, missingImageNote)
			case len(img.Data) > maxImageBytes:
				text = append(text, oversizeImageNote)
			default:
				resolved = append(resolved, img)
			}
		}
	}
	return strings.Join(text, "\n"), resolved, nil
}

// asJSONString keeps a string value as-is and serializes any other JSON value (object,
// number, null) to its compact JSON, so a canonical string field never rejects the
// object shapes the engine uses for content and arguments.
func asJSONString(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
}

// toEngineToolCalls resolves each provider tool call's arguments — a JSON string — to
// an object exactly once. This is the single boundary where the string becomes the
// object the engine wire (and engine.schema.json $defs/tool_call) requires.
func toEngineToolCalls(calls []modelbroker.ToolCall) ([]contracts.ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]contracts.ToolCall, 0, len(calls))
	for _, c := range calls {
		args := map[string]any{}
		if c.Arguments != "" {
			if err := json.Unmarshal([]byte(c.Arguments), &args); err != nil {
				return nil, fmt.Errorf("tool call %q arguments are not a JSON object", c.Name)
			}
		}
		out = append(out, contracts.ToolCall{ID: c.ID, Name: c.Name, Arguments: args})
	}
	return out, nil
}

func addUsage(a, b contracts.Usage) contracts.Usage {
	return contracts.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
		ToolCalls:        a.ToolCalls + b.ToolCalls,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
	}
}

// effectiveModel resolves the model id this step routes under (spec §9.3, §25.16). A session
// config revision's model wins; absent one, the project's DB-backed route (E13 T8); absent that,
// the deployment default.
func (o *Orchestrator) effectiveModel(ctx context.Context, st *attemptState) (string, error) {
	// A ChildRun routes its own model id (spec §25.18): the cheaper/alias model the delegation
	// asked for, within its parent's route (the child shares the project, so it shares the
	// connection). It wins over every lower layer so a child can run a different model than its parent.
	if st.childModel != "" {
		return st.childModel, nil
	}
	override, found, err := o.spine.LatestSessionConfig(ctx, st.tenant, st.sessionID)
	if err != nil {
		return "", err
	}
	if found && override.Model != "" {
		return override.Model, nil
	}
	// A pinned agent/template revision routes its model when neither a child nor a session override set
	// one (spec §14.3 profile+overrides). The pin is immutable on the run row (AGT-001).
	if pinned, err := o.spine.PinnedExecConfig(ctx, st.tenant, string(st.attempt.RunID)); err != nil {
		return "", err
	} else if pinned.Model != "" {
		return pinned.Model, nil
	}
	route, err := o.effectiveRoute(ctx, st)
	if err != nil {
		return "", err
	}
	return route.Model, nil
}

// advertisedTools resolves the run's effective tool set at this boundary and maps each effective
// name the broker actually registers to its advertised schema (spec §14, §28.5, E12 T1). It reads
// the SAME three config layers effectiveConfigHash resolves through (session override, project
// baseline, pinned agent/template revision), so the advertised set never diverges from the config
// the run is checkpointed under. A name the effective set carries but the broker does not register
// is silently dropped — the model is never offered a tool the broker cannot execute (broker.Execute
// returns ErrUnknownTool for it). An empty effective set yields nil, leaving the request unchanged.
func (o *Orchestrator) advertisedTools(ctx context.Context, st *attemptState, pinned coordinator.PinnedConfig) ([]modelbroker.ToolSchema, []SkillRef, error) {
	override, _, err := o.spine.LatestSessionConfig(ctx, st.tenant, st.sessionID)
	if err != nil {
		return nil, nil, err
	}
	policy, err := o.spine.ProjectConfig(ctx, st.tenant)
	if err != nil {
		return nil, nil, err
	}
	route, err := o.effectiveRoute(ctx, st)
	if err != nil {
		return nil, nil, err
	}
	in := ResolveInput{
		ProjectTools:              policy.DefaultTools,
		AgentRevisionID:           pinned.RevisionID,
		AgentRevisionModel:        pinned.Model,
		AgentRevisionTools:        pinned.Tools,
		AgentRevisionToolSetTools: pinned.ToolSetTools,
		SkillPinsJSON:             pinned.SkillPins,
		SessionModel:              override.Model,
		SessionTools:              override.Tools,
	}
	o.routeLayers(route, &in)
	snap := Resolve(in)
	// SchemaResolved checks the static broker set first, then the per-tenant registry lookup — so a
	// REGISTERED tool (an MCP/remote executor) the broker can execute but that is not code-defined is
	// advertised too (E12 T5). Without this fallback a registry tool is resolvable-at-dispatch but never
	// advertised, so the model never spontaneously calls it. The lookup chain only resolves published +
	// pinned tools, so a draft (untrusted, unapproved description) is never advertised (EXT-006). The env
	// carries the run scope the tenant-scoped lookup needs.
	env := o.execEnv(st)
	var advertised []modelbroker.ToolSchema
	for _, name := range snap.Tools {
		tool, ok, err := o.tools.SchemaResolved(ctx, env, name)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue // an effective-set name the broker cannot execute or resolve is not offered
		}
		advertised = append(advertised, modelbroker.ToolSchema{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.InputSchema,
		})
	}
	// The run's frozen skills ride alongside the advertised tools (spec §28.16). They are NOT tools — the
	// caller prepends them as a single progressive-loading system message (metadata only; the body reads
	// on-demand via the file tool). Returned from the SAME resolve, so a skill NEVER diverges from the
	// config the run is checkpointed under.
	return advertised, snap.Skills, nil
}

// skillContextMessage renders the run's frozen skills into ONE system message (spec §28.16, progressive
// loading): each skill's NAME + DESCRIPTION + workspace PATH — the metadata rider, NEVER the body. The
// model reads a skill's full instructions on-demand from Path via the file tool. This is a CP-side
// message prepend onto the engine-assembled conversation; it adds NO engine frame/command kind and
// grants NO capability (TOL-011). Empty skills yields the zero Message (the caller skips it).
func skillContextMessage(skills []SkillRef) modelbroker.Message {
	if len(skills) == 0 {
		return modelbroker.Message{}
	}
	var b strings.Builder
	b.WriteString("You have the following skills available. Each is a set of instructions you can apply. ")
	b.WriteString("Read a skill's full instructions on demand from its workspace path using the file tool BEFORE applying it; ")
	b.WriteString("a skill grants no new capability — it cannot let you call a tool you are not already permitted to use.\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "- %s: %s (read: %s)\n", s.Name, s.Description, s.Path)
	}
	return modelbroker.Message{Role: "system", Content: b.String()}
}

// watchInterrupt polls for a pending interrupt while a model call is outstanding and cancels
// the call if one arrives (the §25.11 in-flight-abort controller half). It reports exactly one
// interruptHit when modelCtx ends: found when it aborted the call, empty when the caller
// canceled it because the call returned first. The poll uses modelCtx, so a normal return that
// cancels it makes any in-flight poll fall through to the done branch.
func (o *Orchestrator) watchInterrupt(ctx context.Context, st *attemptState, cancel context.CancelFunc, out chan<- interruptHit) {
	ticker := time.NewTicker(interruptPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			out <- interruptHit{}
			return
		case <-ticker.C:
			pending, found, err := o.spine.PendingInterruptCommand(ctx, st.tenant, string(st.attempt.RunID))
			if err == nil && found {
				message := ""
				if pending.Kind == "send_message" {
					message = decodeMessage(pending.Payload)
				}
				out <- interruptHit{found: true, commandID: pending.CommandID, kind: pending.Kind, message: message, payload: pending.Payload}
				cancel()
				return
			}
		}
	}
}

// handleInterrupt ends an interrupt-aborted model step and resumes the run in a new one. It
// records the partial step — carrying the streamed-so-far output as the explicit partial item
// (spec §25.16), not None — applies the interrupt command atomically, then tells the engine the
// step was interrupted so it re-requests the model. A send_message folds its delivered message
// into the new step; a change_config applies its revision so the new step routes under the new
// config, plus a warning (spec §9.2, §9.3). The synthetic model.result is ALWAYS sent once the
// call was aborted — otherwise the engine, still awaiting a result, would hang — and carries the
// partial output so the engine records it as the partial assistant turn. A command a boundary
// already applied (the raced degraded path) skips the settle but still resumes the engine.
func (o *Orchestrator) handleInterrupt(ctx context.Context, st *attemptState, frame contracts.EngineFrame, requestID string, hit interruptHit, partialOutput string) error {
	partialData := map[string]any{"run_id": st.attempt.RunID, "model_request_id": requestID}
	if partialOutput != "" {
		partialData["output"] = partialOutput
	}
	partial, _ := json.Marshal(partialData)

	if hit.kind == "change_config" {
		if err := o.applyImmediateConfigChange(ctx, st, hit, requestID, partial); err != nil {
			return err
		}
	} else {
		// The aborted step's model_request_id keys the durable interrupt-delivered row (spec §26.9,
		// ENG-012): a reconstructing attempt refolds the message at THIS same boundary, interleaved with
		// any boundary-delivered message by applied_sequence.
		switch _, err := o.spine.InterruptModelStep(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), hit.commandID, requestID, eventModelStepInterrupted, partial); {
		case errors.Is(err, coordinator.ErrCommandNotPending):
			// Already applied by a boundary; the message is delivered — just resume the engine.
		case err != nil:
			return err
		default:
			deliver := o.frame(st, "message.deliver", map[string]any{"command_id": hit.commandID, "delivery": "interrupt", "message": hit.message}, "")
			if err := st.ch.Send(ctx, deliver); err != nil {
				return err
			}
		}
	}

	resultData := map[string]any{"model_request_id": requestID, "interrupted": true}
	if partialOutput != "" {
		resultData["output"] = partialOutput
	}
	interrupted := o.frame(st, "model.result", resultData, string(frame.ID))
	return st.ch.Send(ctx, interrupted)
}

// applyImmediateConfigChange settles an immediate config switch that aborted the in-flight step
// (spec §9.3, §25.16): it resolves the new ConfigSnapshot, records the partial step, applies the
// change (the config revision + config.revised.v1), and raises a warning — atomically. The
// resumed model step then re-resolves and routes under the new config. A change a boundary
// already applied returns ErrCommandNotPending and is a no-op.
func (o *Orchestrator) applyImmediateConfigChange(ctx context.Context, st *attemptState, hit interruptHit, requestID string, partial []byte) error {
	plan, err := o.planConfigChange(ctx, st, hit.commandID, hit.payload)
	if err != nil {
		return err
	}
	warning, _ := json.Marshal(map[string]any{"command_id": hit.commandID, "code": "config_switch_interrupted", "detail": "the in-flight model step was interrupted for an immediate config switch"})
	switch _, err := o.spine.InterruptForConfigChange(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), plan, hit.commandID, requestID, eventModelStepInterrupted, partial, eventWarningRaised, warning); {
	case errors.Is(err, coordinator.ErrCommandNotPending):
		return nil // already applied by a boundary; the resumed step still picks up the config
	default:
		return err
	}
}
