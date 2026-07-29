package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// The Slack interactivity receiver (E19 T2, spec §36, SLK-004/006/007). It is the events route's sibling and
// rides the same unauthenticated top mux for the same reason: its auth IS the per-request v0 signature.
//
// THE ONE THING THAT MAKES THIS ROUTE DIFFERENT FROM ITS SIBLING, and it is the whole security posture:
//
//	CONTRACT https://docs.slack.dev/interactivity/handling-user-interaction/ (checked 2026-07-26) — an
//	interactivity payload arrives "in the form application/x-www-form-urlencoded" and "The body of the
//	request will contain a payload parameter; your app should parse this payload parameter as JSON". A 200
//	is owed "within 3 seconds of receiving the payload" (documented HERE, unlike Socket Mode, where no such
//	number is published — see plan §3.5 D4).
//
//	THE INFERENCE, with its source, so a later reader does not have to re-derive it: that page does NOT say
//	the signature must be verified over the raw form body before deserialization. It says nothing about
//	verification at all. The requirement comes from the OTHER page,
//	https://docs.slack.dev/authentication/verifying-requests-from-slack/ (checked 2026-07-26), whose base
//	string is built from "the raw request body, before it has been deserialized". Composing the two: the
//	bytes Slack MACs are `payload=%7B%22type%22...`, NOT the JSON inside. Verifying the extracted JSON checks
//	a string Slack never signed, so no honest signature could ever match it — meaning a receiver written that
//	way can only appear to work by not checking at all. The order below is forced by that composition.
//
// ORDER: raw body → size cap → extract the single payload → pre-parse the team (LOOKUP KEY, never trust) →
// resolve the connection → VERIFY over the RAW BODY → map → decide → 200.
//
// Why extraction precedes resolution here while the events route parses the team straight out of the body:
// the team id lives INSIDE the url-encoded payload, so there is no way to learn which secret to verify
// against without url-decoding first. Decoding is not trusting — the decoded value selects a candidate
// connection, and that connection's secret must then MAC the RAW bytes or nothing happens.
//
// NO RETRY SEMANTICS HERE, deliberately: x-slack-no-retry belongs to the Events API's three-redelivery rule
// (plan §3.5 D1). Interactivity has no documented redelivery — the consequence of a non-200 is that the
// clicking user sees an error — so this route sets no suppress header, and a reader looking for the sibling's
// D1 discipline finds this sentence instead of an unexplained absence.

// slackInteractionAckBudget bounds everything this handler does before it answers, so a stuck dependency
// becomes an honest 503 rather than a hung request and a user staring at a spinner. Two seconds against the
// documented three leaves headroom for the round trip we do not control.
const slackInteractionAckBudget = 2 * time.Second

// SlackDecisionOutcome is one interactive decision's result. Rejected names a TYPED refusal ("" when the
// decision was applied): an unmapped clicker, a thread with no session, a hash that no longer matches the
// pending approval. Every one of them means ZERO commands were enqueued.
type SlackDecisionOutcome struct {
	SessionID     string
	PublicationID string
	CommandID     string
	Decision      string // "approve" | "deny" — the decision that was actually applied
	Rejected      string
	// Repaired reports whether the visible Slack message was edited to reflect the outcome. FALSE is not a
	// failure of the decision: SLK-006's invariant is that a Slack delivery problem never erases canonical
	// state, so the decision is durable either way and only the message is stale.
	Repaired bool
}

// SlackInteractionsAPI is the narrow seam this route drives (implemented by extensions.SlackAdmitter once its
// decision half is wired). Split three ways for the same reason as SlackEventsAPI: it lets a test observe
// that nothing was verified before a connection resolved, and nothing decided before a signature verified.
type SlackInteractionsAPI interface {
	// ResolveConnection establishes the tenant for an UNAUTHENTICATED callback from the untrusted pre-parsed
	// team id. found=false covers "unknown workspace" with no further detail (no config oracle).
	ResolveConnection(ctx context.Context, teamID, enterpriseID string) (SlackConnectionRef, bool, error)
	// VerifySignature checks the v0 MAC over the RAW request body under the connection's signing_secret_ref.
	VerifySignature(ctx context.Context, conn SlackConnectionRef, timestamp, signature string, body []byte) error
	// Decide runs the authorization + one-shot approval chain for a VERIFIED interactive approval, scoped
	// ONLY by the resolved connection's org/project.
	Decide(ctx context.Context, conn SlackConnectionRef, intent slack.ApprovalIntent) (SlackDecisionOutcome, error)
	// OpenApprovalArguments re-derives the approval screen from the ledger and opens it as a modal (E23 T4).
	// It runs the SAME authorization chain Decide runs and DECIDES NOTHING; the string it returns is a typed
	// refusal ("" when the modal opened), and an error means the receiver itself failed.
	//
	// IT IS CALLED INSIDE THE ACK BUDGET AND THAT IS THE CONTRACT, not an implementation note: a trigger_id
	// expires three seconds after Slack sends it (https://docs.slack.dev/surfaces/modals/, §3.5 P10, fetched
	// 2026-07-29) and the 200 below is owed in the same three seconds. Deferring this to a goroutine would
	// answer Slack faster and open nothing.
	OpenApprovalArguments(ctx context.Context, conn SlackConnectionRef, intent slack.ShowArgumentsIntent) (string, error)
}

// slackInteractionsHandler serves POST /v1/slack/interactions.
type slackInteractionsHandler struct {
	slack  SlackInteractionsAPI
	budget time.Duration
	logf   func(string, ...any)
}

func (h *slackInteractionsHandler) log(format string, args ...any) {
	if h.logf != nil {
		h.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (h *slackInteractionsHandler) receive(w http.ResponseWriter, r *http.Request) {
	// 1. RAW bytes, bounded BEFORE anything hashes or decodes them. These exact bytes are what the MAC covers.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "the interaction exceeds the size limit")
		return
	}

	budget := h.budget
	if budget <= 0 {
		budget = slackInteractionAckBudget
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()

	// 2. The documented body shape. A JSON body lands here — Slack does not send one, and accepting it would
	// mean the MAC below was computed over something other than what Slack signed.
	payload, err := slack.ExtractInteractionPayload(body)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"the interaction body is not a url-encoded payload parameter")
		return
	}

	// 3. The lookup key, from inside the payload. Untrusted: it selects a candidate connection, nothing more.
	teamID, enterpriseID, ok := slack.ParseInteractionTeam(payload)
	if !ok {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the interaction payload is malformed")
		return
	}
	conn, found, err := h.slack.ResolveConnection(ctx, teamID, enterpriseID)
	switch {
	case err != nil:
		h.log("slack interactions: resolve connection failed") // no team id / no error text — neither is ours to leak
		middleware.WriteProblem(w, r, http.StatusServiceUnavailable, "internal_error", "the receiver is temporarily unavailable")
		return
	case !found || conn.Disabled:
		// One generic 404 for every disqualifier, the events route's posture: an unauthenticated prober learns
		// nothing about a workspace it could not authenticate.
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such Slack endpoint")
		return
	}

	// 4. Authenticate — over the RAW form body, never the extracted payload (see the header comment).
	if err := h.slack.VerifySignature(ctx, conn, r.Header.Get(slack.HeaderTimestamp), r.Header.Get(slack.HeaderSignature), body); err != nil {
		h.log("slack interactions: signature rejected: connection=%s reason=%v", conn.ID, err) // typed reason only
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "invalid_signature", "the interaction signature did not verify")
		return
	}

	// 5a. THE MODAL BRANCH, AND ITS POSITION IN THIS FUNCTION IS THE THREE-SECOND RULE (E23 T4, §3.5 P10).
	// It sits after authentication and BEFORE the 200, because `trigger_id` dies three seconds after Slack
	// sends it and the ack is owed inside the same three seconds — so views.open has to happen here, in the
	// budget, or not at all. Handing it to a goroutine would ack faster and open nothing.
	//
	// It runs before the approval mapping only because the two are disjoint by action id; a Show-arguments
	// click can never map to an approval (MapInteractiveApproval's switch refuses it) and an approve click
	// can never map to a modal request. Neither ordering can leak into the other.
	if show, serr := slack.MapShowArgumentsClick(payload); serr == nil {
		rejected, oerr := h.slack.OpenApprovalArguments(ctx, conn, show)
		if oerr != nil {
			h.log("slack interactions: opening the argument modal failed: connection=%s", conn.ID)
			middleware.WriteProblem(w, r, http.StatusServiceUnavailable, "internal_error", "the receiver is temporarily unavailable")
			return
		}
		if rejected != "" {
			// 200 with nothing opened, the same asymmetry a refused decision has: telling the clicker would
			// make "you are not an approver" a signal an unmapped user can read off the UI.
			h.log("slack interactions: argument modal refused: connection=%s reason=%s", conn.ID, rejected)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// 5. Map — strictly after authentication. An interaction that is not one of OUR minted approve/deny
	// buttons carrying the one-shot hash authorizes nothing and reaches no decision path; it is still
	// acknowledged, because the delivery succeeded and a non-200 would show the clicking user an error for
	// something we simply have no opinion about.
	intent, err := slack.MapInteractiveApproval(payload)
	if errors.Is(err, slack.ErrNotApproval) {
		h.log("slack interactions: ignored a non-approval interaction on connection=%s", conn.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the interaction payload is malformed")
		return
	}

	// 6. Decide: authorization, then the one-shot approval bound to the request hash.
	out, err := h.slack.Decide(ctx, conn, intent)
	if err != nil {
		h.log("slack interactions: decision failed: connection=%s action=%s", conn.ID, intent.ActionID)
		middleware.WriteProblem(w, r, http.StatusServiceUnavailable, "internal_error", "the receiver is temporarily unavailable")
		return
	}
	if out.Rejected != "" {
		// 200 WITH NOTHING DONE, and the asymmetry is deliberate: a refusal is recorded control-plane-side and
		// the clicker is told nothing. Answering non-200 would turn "you are not an approver" into a signal an
		// unmapped user can read off the UI — the generic-404 posture, one layer in.
		//
		// CEILING, stated where a reader meets it: the clicker therefore sees NO feedback at all. Telling them
		// would mean an ephemeral reply, which needs either response_url (30 minutes / 5 uses — the wrong tool,
		// see MapInteractiveApproval) or chat.postEphemeral; a considered approval UI is the console's job.
		h.log("slack interactions: decision refused: connection=%s user=%s reason=%s", conn.ID, intent.UserID, out.Rejected)
		w.WriteHeader(http.StatusOK)
		return
	}
	h.log("slack interactions: decided connection=%s decision=%s session=%s publication=%s command=%s repaired=%t",
		conn.ID, out.Decision, out.SessionID, out.PublicationID, out.CommandID, out.Repaired)
	w.WriteHeader(http.StatusOK)
}
