package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// The Slack Events API receiver (E19 T1, spec §36, SLK-001..005/008). Like the signed inbound-webhook
// receiver and the remote-tool callback it mounts on the UNAUTHENTICATED top mux, because its auth IS the
// per-request v0 signature — Slack presents no bearer. Everything durable it does goes through the SAME
// admission the native POST /v1/responses takes; this file invents no run identity and no second dedupe.
//
// THE ORDER BELOW IS THE SECURITY POSTURE, and each step is forced by the published contract rather than
// chosen for convenience:
//
//  1. Read the RAW body under a size cap. The MAC is computed over these exact bytes, so nothing may
//     deserialize, re-encode or normalize them first, and the cap must precede the hashing or an unsigned
//     caller can make us HMAC gigabytes.
//  2. ParseChallenge. CONTRACT https://docs.slack.dev/apis/events-api/ (checked 2026-07-25): configuring a
//     Request URL makes Slack POST {"type":"url_verification","challenge":…}, answered by echoing the
//     challenge in plaintext. It runs BEFORE verification because that body carries NO team_id — there is no
//     connection to resolve and therefore no signing secret to verify against. The order is forced.
//  3. Resolve the connection from the PRE-PARSED team id. Untrusted: a lookup key, never an identity. What
//     makes it safe is step 4 — the resolved connection's secret must verify the signature over this very
//     body, so a forged team_id only selects a connection whose secret the forger does not hold.
//  4. VerifySignature. CONTRACT https://docs.slack.dev/authentication/verifying-requests-from-slack/
//     (checked 2026-07-25): base string exactly 'v0:'+timestamp+':'+body, HMAC-SHA256, the header value
//     'v0='-prefixed, the comparison timing-safe, and a timestamp more than five minutes from local time
//     refused. The adapter has done all of that since E17 T1; this route's job is not to bypass it.
//  5. MapEvent — strictly AFTER authentication, never before.
//  6. Reserve the dedupe (which IS the admission — see below) and only then answer 2xx.
//
// WHY THE RESERVATION IS SYNCHRONOUS, stated plainly because the plan's step list reads "reserve → ack → work
// async": E19 is migration-free, and the ONE durable, atomic, migration-free reservation in this tree is the
// idempotency record the admission transaction takes. So "reserve the dedupe" and "admit" are the same
// commit, and it lands BEFORE the ack — because SLK-001/002 (one effect per source event, even when Slack
// redelivers) outranks shaving a few milliseconds off an ack that is one bounded DB transaction. The work
// that actually takes time — running the agent — IS asynchronous, and by architecture rather than by a
// goroutine here: admission only ENQUEUES a run and the dispatch workers execute it (§34.2). What guards the
// budget is slackAckBudget below, not a fire-and-forget.
//
// CEILING: a durable per-delivery Slack record (retry reason, ack latency) would need a table, i.e. a
// migration, which this phase does not take. The retry reason is therefore recorded in-process and logged;
// see slackRetryLedger.

const (
	// slackAckBudget bounds everything this handler does before it answers. CONTRACT
	// https://docs.slack.dev/apis/events-api/ (checked 2026-07-25): a delivery Slack has not seen a 2xx for
	// within THREE seconds is considered failed and retried. Two seconds leaves headroom for the round trip
	// we do not control. Missing it is TRANSIENT, never terminal — the response carries no suppress header,
	// so Slack retries and the retry collapses on the dedupe reservation.
	slackAckBudget = 2 * time.Second

	// headerSlackNoRetry suppresses Slack's redelivery of a response it would otherwise treat as failed.
	// CONTRACT https://docs.slack.dev/apis/events-api/ (checked 2026-07-25): three retries (immediately,
	// +1 minute, +5 minutes) unless the response carries "x-slack-no-retry: 1". Plan §3.5 row D1.
	headerSlackNoRetry = "X-Slack-No-Retry"
)

// SlackConnectionRef is what an inbound callback resolves to before it has a tenant: the registered
// workspace binding's identity, the two fields the route itself reads, and the material the bridge needs on
// the later calls. It carries NO secret VALUES — SigningSecretRef is a handle into secret_refs, and the bytes
// behind it are redeemed inside the bridge, used, and dropped. Carrying the handle (rather than re-resolving
// the row per step) is deliberate: this route is UNAUTHENTICATED, so one callback must cost exactly ONE
// connection read or the resolve itself becomes an amplification surface (storage/queries/slack.sql says so).
type SlackConnectionRef struct {
	ID           string
	Project      string
	TeamID       string
	EnterpriseID string
	BotUserID    string // the app's own bot user — the SLK-008 self-loop guard
	Disabled     bool
	// SigningSecretRef is the secret_refs HANDLE the v0 verify redeems. A handle, never a value.
	SigningSecretRef string
	// BotTokenRef is the secret_refs HANDLE the outbound chat.update redeems (E19 T2). Also a handle: the
	// token bytes are resolved inside the bridge at call time, ride the Authorization header, and are dropped.
	// It rides the same single connection read for the same reason SigningSecretRef does.
	BotTokenRef string
	// AppTokenRef is the secret_refs HANDLE for the app-level (xapp-) token the Socket Mode connect loop
	// presents to apps.connections.open (E19 T3). A handle like the other two. It matters more than they do
	// that it stay one: Socket Mode's ONLY authentication is this token at connect time — the frames it then
	// carries are unsigned (https://docs.slack.dev/apis/events-api/using-socket-mode/) — so a leaked app
	// token is the whole transport. It rides the same single connection read.
	AppTokenRef string
	// RunPolicy is the connection's default_policy JSONB, opaque to this package: the route hands it back to
	// the bridge, which reads the run target out of it. Nothing here interprets it.
	RunPolicy []byte
}

// SlackAdmitOutcome is one event's admission result. Rejected names a TYPED admission refusal ("" when the
// run was admitted or the reservation replayed); Retryable says whether that refusal is worth a redelivery —
// a full queue is, a draft revision pin never will be.
type SlackAdmitOutcome struct {
	ResponseID string
	SessionID  string
	Replayed   bool
	Rejected   string
	Retryable  bool
	// Ignored is an event that was HANDLED and birthed no run. Two different things arrive under it:
	//
	//   - ORDINARY CHANNEL CHATTER, which this app subscribes to (SLK-002/SLK-005 need message.channels) and
	//     only acts on when it is addressed — a mention, or a follow-up in a thread the app is already in.
	//   - AN EDIT OR A DELETION, which changes a turn instead of opening one: a correction supersedes the
	//     stored turn and a tombstone retracts it (extensions.reviseTurn). Those DID do something durable, and
	//     it is still not a run — this field says "no run", not "no effect".
	//
	// Both are the same thing to this route: ack, and answer nothing into the thread. See
	// extensions.slackBirthsRun for the rule.
	//
	// It is NOT a Rejected value, and the difference is the log: a refusal is news an operator acts on, while
	// every message two colleagues exchange in the channel would be one line each of it. Ignored is acked and
	// silent here — a retraction that DID land is logged by the bridge, which is the layer that knows one
	// happened. (ErrNoRun is the same idea one layer up, for events the mapping alone can classify.)
	Ignored bool
}

// SlackEventsAPI is the narrow seam this route drives (implemented by extensions.SlackAdmitter). Splitting it
// three ways is what lets the handler's ORDER be the tested contract: a test can observe that nothing was
// verified before a connection resolved and nothing was mapped before a signature verified.
type SlackEventsAPI interface {
	// ResolveConnection establishes the tenant for an UNAUTHENTICATED callback from the untrusted pre-parsed
	// team id. found=false covers "unknown workspace" with no further detail (no config oracle).
	ResolveConnection(ctx context.Context, teamID, enterpriseID string) (SlackConnectionRef, bool, error)
	// VerifySignature checks the v0 MAC over the RAW body under the connection's signing_secret_ref. The
	// resolved bytes live and die inside the implementation.
	VerifySignature(ctx context.Context, conn SlackConnectionRef, timestamp, signature string, body []byte) error
	// Admit reserves the source-event dedupe and births the run through the REAL admission path, scoped ONLY
	// by the resolved connection's org/project.
	Admit(ctx context.Context, conn SlackConnectionRef, ev slack.Event) (SlackAdmitOutcome, error)
}

// slackHandler serves POST /v1/slack/events.
type slackHandler struct {
	slack   SlackEventsAPI
	retries slackRetryLedger
	logf    func(string, ...any)
}

func (h *slackHandler) log(format string, args ...any) {
	if h.logf != nil {
		h.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// receive ingests one Events API callback in the order documented at the top of this file.
func (h *slackHandler) receive(w http.ResponseWriter, r *http.Request) {
	// 1. RAW bytes, bounded at the trust boundary BEFORE anything hashes or parses them.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		slackTerminal(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "the event exceeds the size limit")
		return
	}

	// 2. The url_verification handshake — answered before (and without) any lookup, because it carries no
	// team_id. That also makes it the ONE surface here that renders caller-chosen bytes on this deployment's
	// own origin, unauthenticated, so the echo is bounded to what a real handshake can be: a short
	// alphanumeric token, served as declared-inert text that a browser may not sniff into a document.
	// Anything else is a reflection surface rather than a handshake, and is refused terminally.
	if challenge, ok := slack.ParseChallenge(body); ok {
		if !validSlackChallenge(challenge) {
			slackTerminal(w, r, http.StatusBadRequest, "invalid_request", "the url_verification challenge is not a valid token")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(challenge))
		return
	}

	// Everything from here is bounded by the ack budget, so a slow dependency answers Slack in time with a
	// retryable status instead of silently blowing the three-second window.
	ctx, cancel := context.WithTimeout(r.Context(), slackAckBudget)
	defer cancel()

	// 3. The lookup key. No team id ⇒ no connection ⇒ no secret ⇒ nothing can ever authenticate this body:
	// terminal, and retrying it three times changes nothing.
	teamID, enterpriseID, ok := slack.ParseTeam(body)
	if !ok {
		slackTerminal(w, r, http.StatusBadRequest, "invalid_request", "the event envelope is malformed")
		return
	}
	conn, found, err := h.slack.ResolveConnection(ctx, teamID, enterpriseID)
	switch {
	case err != nil:
		// Infrastructure, not the sender's fault: invite the retry (no suppress header).
		h.log("slack events: resolve connection failed") // no team id / no error text — neither is ours to leak
		middleware.WriteProblem(w, r, http.StatusServiceUnavailable, "internal_error", "the receiver is temporarily unavailable")
		return
	case !found || conn.Disabled:
		// One generic 404 for every disqualifier, the inbound-receiver posture: an unauthenticated prober
		// learns nothing about a workspace it could not authenticate. Terminal — a redelivery cannot register
		// the connection.
		slackTerminal(w, r, http.StatusNotFound, "not_found", "no such Slack endpoint")
		return
	}

	// 4. Authenticate. A stale timestamp and a bad MAC are both terminal: the five-minute window will not
	// re-open and the signature will not become valid.
	if err := h.slack.VerifySignature(ctx, conn, r.Header.Get(slack.HeaderTimestamp), r.Header.Get(slack.HeaderSignature), body); err != nil {
		h.log("slack events: signature rejected: connection=%s reason=%v", conn.ID, err) // typed reason only
		slackTerminal(w, r, http.StatusUnauthorized, "invalid_signature", "the event signature did not verify")
		return
	}

	// 5. Redelivery bookkeeping (plan §3.5 D2) — recorded only for an AUTHENTICATED request, so an
	// unauthenticated flood cannot move the counter that is supposed to measure our own ack latency.
	retryNum := r.Header.Get(slack.HeaderRetryNum)
	if reason := slack.RetryReason(r.Header.Get(slack.HeaderRetryReason)); reason != "" || retryNum != "" {
		h.log("slack events: redelivery connection=%s attempt=%s reason=%s http_timeouts=%d",
			conn.ID, retryNum, reason, h.retries.record(reason))
	}

	// 5b. Slack telling us we are being THROTTLED (plan §3.5 D10). It is an OUTER type, not an event_callback,
	// so MapEvent below refuses it and the route would answer 400 + the suppress header — discarding the one
	// notification Slack sends about our own delivery budget. It is handled AFTER verification like everything
	// else: an unauthenticated caller must not be able to write our operational counters.
	//
	// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — Events API delivery is
	// capped at 30,000 per workspace/app per 60 minutes; past that the app receives `app_rate_limited` carrying
	// team_id and minute_rate_limited. There is nothing to DO about it in-process (the sender is Slack), so the
	// honest handling is a log + a counter an operator can see, which is what D10 asks for.
	if _, minute, ok := slack.ParseAppRateLimited(body); ok {
		h.log("slack events: THROTTLED by Slack — connection=%s minute_rate_limited=%d total=%d (deliveries are being dropped upstream; https://docs.slack.dev/apis/web-api/rate-limits/)",
			conn.ID, minute, h.retries.count(slackThrottleCounter))
		w.WriteHeader(http.StatusOK)
		return
	}

	// 6. Map — strictly after authentication.
	ev, err := slack.MapEvent(body, conn.BotUserID, retryNum != "")
	switch {
	case errors.Is(err, slack.ErrIgnored):
		// SLK-008: our own bot's message (or our own chat.update re-entering as a nested message_changed).
		// The DELIVERY succeeded, so it is acked; it simply produces nothing. Acking is also what stops
		// Slack redelivering an event we will keep ignoring.
		w.WriteHeader(http.StatusOK)
		return
	case errors.Is(err, slack.ErrNoRun):
		// E20 T2's agent panel surface: app_home_opened / app_context_changed. HANDLED but not conversation —
		// acked so Slack stops redelivering, and no run. Socket Mode's dispatch does the identical thing; the
		// two transports may not disagree about which surfaces are conversation. tab=="home" and
		// tab=="messages" are both no-run, so the tab is logged rather than branched on.
		h.log("slack events: acknowledged panel surface event %q (tab=%q channel=%s) on connection %s and birthed no run",
			ev.Type, ev.Tab, ev.ChannelID, conn.ID)
		w.WriteHeader(http.StatusOK)
		return
	case err != nil:
		slackTerminal(w, r, http.StatusBadRequest, "invalid_request", "the event envelope is malformed")
		return
	}

	// 7. Reserve the dedupe + admit (one commit, see the header comment), then ack.
	out, err := h.slack.Admit(ctx, conn, ev)
	switch {
	case err != nil:
		// THE ERROR IS NAMED. It was not, and the cost was measured on 2026-08-05: seventy-two tests in this
		// family failed as a bare "503, want a 2xx ack" with the reason discarded here, so every one of them
		// reported the symptom of an admission failure and none of them its cause. The body stays generic —
		// an outsider learns nothing — but the OPERATOR's log says what broke.
		h.log("slack events: admission failed: connection=%s event=%s: %v", conn.ID, ev.SourceEventID, err)
		middleware.WriteProblem(w, r, http.StatusServiceUnavailable, "internal_error", "the receiver is temporarily unavailable")
		return
	case out.Ignored:
		// Channel chatter the app is not addressed in. Acked (Slack delivered it correctly, and refusing would
		// only earn three redeliveries of the same sentence) and NOT logged: the quiet is the feature. Socket
		// Mode's dispatch does the identical thing.
		w.WriteHeader(http.StatusOK)
		return
	case out.Rejected != "" && out.Retryable:
		// Load, not poison: a full queue empties. Retry-After pairs with the 429 the way §20.12 does it.
		w.Header().Set("Retry-After", "60")
		middleware.WriteProblem(w, r, http.StatusTooManyRequests, "rate_limited", out.Rejected)
		return
	case out.Rejected != "":
		// Configuration/poison: a draft revision pin, a closed session, a reused key. Three more deliveries
		// produce three more identical refusals.
		slackTerminal(w, r, http.StatusUnprocessableEntity, "invalid_request", out.Rejected)
		return
	}
	// The ack is earned: the reservation is committed and the run is queued for the dispatch workers. Logged
	// with the outcome so an operator can tell a fresh admission from a redelivery that replayed onto it.
	h.log("slack events: admitted connection=%s event=%s response=%s session=%s replayed=%t",
		conn.ID, ev.SourceEventID, out.ResponseID, out.SessionID, out.Replayed)
	w.WriteHeader(http.StatusOK)
}

// validSlackChallenge bounds what the handshake will echo back.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — the challenge is a short random
// token (the documented example is 50-odd alphanumerics). Nothing says it is alphanumeric FOREVER, so this is
// the deliberate trade: a token shaped like every one Slack has ever sent is echoed, and anything else is
// refused rather than reflected. If Slack ever widens the alphabet the handshake fails loudly at Request-URL
// configuration time — visible immediately, unlike a reflection nobody notices.
func validSlackChallenge(challenge string) bool {
	const maxSlackChallenge = 1024 // ~20x the documented length; a real token is nowhere near it
	if challenge == "" || len(challenge) > maxSlackChallenge {
		return false
	}
	for _, r := range challenge {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// slackTerminal answers a rejection Slack must NOT redeliver (plan §3.5 D1). Every caller is a state a
// redelivery cannot change: an unsignable body, an unknown workspace, a failed MAC, a poison envelope, a
// permanently refused admission. A TRANSIENT failure must never come through here — it writes the problem
// directly, so the retry survives.
func slackTerminal(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	w.Header().Set(headerSlackNoRetry, "1")
	middleware.WriteProblem(w, r, status, code, detail)
}

// slackRetryLedger counts Slack's OWN stated reasons for redelivering to us (plan §3.5 D2). http_timeout is
// the one that matters: it is the only honest indicator that the three-second ack budget is not holding,
// because every other reason means we answered in time and answered badly.
//
// ponytail: in-process counters + a log line per redelivery. A durable per-delivery record needs a table and
// E19 takes no migration; a /metrics series is the upgrade path if an operator needs it aggregated.
type slackRetryLedger struct {
	mu      sync.Mutex
	reasons map[string]int
}

// record counts one redelivery and returns the running http_timeout total, so the log line carries the
// budget signal rather than requiring someone to aggregate it later. reason has already been narrowed to
// Slack's documented set by slack.RetryReason.
func (l *slackRetryLedger) record(reason string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reasons == nil {
		l.reasons = map[string]int{}
	}
	if reason == "" {
		reason = slack.RetryReasonUnknownError // a retry number with no reason header still happened
	}
	l.reasons[reason]++
	return l.reasons[slack.RetryReasonHTTPTimeout]
}

// slackThrottleCounter is the ledger key for app_rate_limited notices. It shares the retry ledger rather than
// growing a second map because it is the same class of fact — Slack telling us something about our own
// delivery health — and it cannot collide: slack.RetryReason narrows a retry reason to six documented values,
// none of them this one.
const slackThrottleCounter = "app_rate_limited"

// count increments an arbitrary ledger key and returns its running total.
func (l *slackRetryLedger) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reasons == nil {
		l.reasons = map[string]int{}
	}
	l.reasons[key]++
	return l.reasons[key]
}

func (l *slackRetryLedger) snapshot() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.reasons))
	for k, v := range l.reasons {
		out[k] = v
	}
	return out
}
