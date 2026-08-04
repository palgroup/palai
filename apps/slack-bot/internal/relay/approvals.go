// This file is Task 10 (2026-08-03 plan): the approval bridge. A gated tool call or a gated
// publication parks a run and journals approval.requested.v1 (packages/coordinator/approvals.go
// RequestToolApproval, packages/coordinator/publication.go RequestPublication); this file turns that
// event into an interactive Slack message, and a click on that message's Approve/Deny button into a
// decision through the SDK's /v1/approvals surface (c.Approvals.List/Approve/Deny — sdks/go/approvals.go).
//
// THE BRIEF'S OWN SKETCH NAMED TWO THINGS THAT DO NOT EXIST, and both are corrected here rather than
// followed, per this tree's own rule that a plan's claims are re-measured, not trusted (see
// docs/superpowers/plans's "a plan claim must be executable"):
//
//  1. There is no slack.BuildApprovalBlocks. What exists and IS exported is
//     slack.ToolApprovalMessage(channel, threadTS string, req ApprovalRequest) []byte
//     (adapters/integrations/slack/interactions.go) — it derives the screen via
//     slack.DeriveApprovalDisplay and slack.ApprovalArgumentRows internally and renders the full
//     chat.postMessage body, buttons included. approvalArgumentTable, mentioned as "exposed" in this
//     task's brief, is in fact unexported (`func approvalArgumentTable` — lowercase — blocks.go);
//     ToolApprovalMessage is the highest-level exported builder and this file uses that rather than
//     re-deriving the table by hand.
//  2. There is no slack.Interaction type. The real output of a verified interactivity payload is
//     slack.ApprovalIntent, produced by slack.MapInteractiveApproval — OnButton takes that instead.
//
// THE THIRD THING THE BRIEF GOT RIGHT AND WORTH RESTATING HERE: ToolApprovalMessage mints a THIRD
// button, "Show arguments" (slack.ActionShowArguments), that opens a modal via views.open. This file
// does not wire a handler for it — OnButton only ever branches on approve/deny — so that button will
// render on every message this file posts and do nothing observable when clicked. It decides nothing
// (MapInteractiveApproval's own doc: "It decides nothing"), so leaving it unhandled is not a security
// gap, only an incomplete affordance; wiring views.open is out of this task's scope.
package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/slack-bot/internal/store"
	palai "github.com/palgroup/palai/sdks/go"
)

// var _ ApprovalPosts / ApprovalThreads = (*store.Store)(nil) pins these two seams to the real store at
// COMPILE time, for the reason inbound.go's identical line records: this package's interfaces and the
// store drifted silently once already, because no production call site wired them together and
// `go build ./...` stayed clean through the whole divergence.
var (
	_ ApprovalPosts   = (*store.Store)(nil)
	_ ApprovalThreads = (*store.Store)(nil)
)

// ApprovalRequestedEventType is the event OnApprovalRequested handles. It is journaled
// byte-identically in name (never in payload shape — see OnApprovalRequested's own doc) for a gated
// tool call and a gated publication. This is this package's OWN copy of the coordinator's unexported
// approvalRequestedEvent constant, mirroring how relay.go's runTerminalEvents already mirrors rather
// than imports the SSE endpoint's own terminal set — the const is unexported on the other side of that
// import edge too.
const ApprovalRequestedEventType = "approval.requested.v1"

// ApprovalsPalai is the /v1/approvals surface OnApprovalRequested/OnButton drive, narrowed from
// *palai.Client's Approvals resource group the same way relay.Palai (inbound.go) narrows
// Sessions/Responses — so a test substitutes a fake with no HTTP round trip.
type ApprovalsPalai interface {
	ListApprovals(ctx context.Context, p palai.ListApprovalsParams) (*palai.Page[palai.Approval], error)
	ApproveApproval(ctx context.Context, id string, p palai.DecisionParams) (*palai.ApprovalDecisionResult, error)
	DenyApproval(ctx context.Context, id string, p palai.DecisionParams) (*palai.ApprovalDecisionResult, error)
}

// approvalsPalaiClient adapts *palai.Client's Approvals resource group to ApprovalsPalai, mirroring
// palaiClient (inbound.go).
type approvalsPalaiClient struct{ c *palai.Client }

// NewApprovalsPalaiClient wraps a real SDK client as the ApprovalsPalai seam this file drives.
func NewApprovalsPalaiClient(c *palai.Client) ApprovalsPalai { return approvalsPalaiClient{c} }

func (p approvalsPalaiClient) ListApprovals(ctx context.Context, params palai.ListApprovalsParams) (*palai.Page[palai.Approval], error) {
	return p.c.Approvals.List(ctx, params)
}

func (p approvalsPalaiClient) ApproveApproval(ctx context.Context, id string, params palai.DecisionParams) (*palai.ApprovalDecisionResult, error) {
	return p.c.Approvals.Approve(ctx, id, params)
}

func (p approvalsPalaiClient) DenyApproval(ctx context.Context, id string, params palai.DecisionParams) (*palai.ApprovalDecisionResult, error) {
	return p.c.Approvals.Deny(ctx, id, params)
}

// ApprovalSlack is the outbound half of the bridge: posting an approval's message and repairing it in
// place once a decision is known — the two chat.* calls this bridge needs, each body already carrying
// its own channel/ts (built by slack.ToolApprovalMessage / slack.UpdateMessage), narrowed the same
// way relay.Slack (relay.go) narrows the same package for the streaming path.
type ApprovalSlack interface {
	// PostMessage sends body (built by buildApprovalMessage) and returns the ts Slack assigns —
	// unused by this file today (a decided click carries its own message ts, see OnButton) but
	// returned rather than discarded because a caller integrating this into a store IS the kind of
	// thing a later task does with it.
	PostMessage(ctx context.Context, body []byte) (ts string, err error)
	// UpdateMessage repairs the message body already names (channel + ts baked in by
	// slack.UpdateMessage) — the SLK-006 single-repair pattern.
	UpdateMessage(ctx context.Context, body []byte) error
}

// webAPIApprovalSlack is ApprovalSlack over the real Slack Web API, mirroring channelSlackStream
// (inbound.go): same Doer/apiBase/token shape, no recipient fields — an approval message is not a
// stream and carries no recipient_user_id/recipient_team_id.
type webAPIApprovalSlack struct {
	doer    slack.Doer
	apiBase string
	token   []byte
}

// NewApprovalSlack builds the production ApprovalSlack.
func NewApprovalSlack(doer slack.Doer, apiBase string, token []byte) ApprovalSlack {
	return &webAPIApprovalSlack{doer: doer, apiBase: apiBase, token: token}
}

func (s *webAPIApprovalSlack) PostMessage(ctx context.Context, body []byte) (string, error) {
	res, err := slack.PostMessage(ctx, s.doer,
		slack.PostRequest{MethodURL: s.apiBase + "/chat.postMessage", Token: s.token, Body: body}, slack.PostOptions{})
	if err != nil {
		return "", err
	}
	return res.MessageTS, nil
}

func (s *webAPIApprovalSlack) UpdateMessage(ctx context.Context, body []byte) error {
	_, err := slack.PostMessage(ctx, s.doer,
		slack.PostRequest{MethodURL: s.apiBase + "/chat.update", Token: s.token, Body: body}, slack.PostOptions{})
	return err
}

// ApprovalDeps is the approval bridge's whole seam.
//
// AllowedApprovers IS THE ONLY PER-HUMAN AUTHORIZATION BOUNDARY THIS PATH HAS, and that is worth
// stating in the struct doc rather than only at the check site. It is resolved by the caller from
// this bot's OWN bots-registry row config (c.Bots.Get) — never an environment variable, mirroring
// InboundDeps's identical rule. The reason it carries this much weight: DecideApproval's deciding
// principal (apps/control-plane/internal/store/approvals.go) is `key:<api_key_id>` — this BOT's own
// API key, the same one for every click this process ever forwards, because Palai has no per-human
// identity to check server-side (known-gaps HIL-P2). The server's ConfigPolicy.ApproverAllowed check
// therefore authorizes THIS BOT AS A WHOLE, once, for its key — it cannot tell one Slack clicker from
// another. Without this list (or with a caller that consulted it too late), every member of the
// channel who can see the message could decide it.
type ApprovalDeps struct {
	Palai ApprovalsPalai
	Slack ApprovalSlack
	// AllowedApprovers are the Slack user ids permitted to decide through this bot.
	AllowedApprovers []string

	// Posts is the durable at-most-once record of which approvals have already been put to a human, and
	// BotID is the key it partitions by. Both are required, because this question now has two producers
	// that overlap by design — the live event arm and SweepApprovals — and without the claim the ordinary
	// case posts twice (see ApprovalPosts).
	Posts ApprovalPosts
	BotID string
}

// ApprovalPosts is the claim this bridge takes before it asks a human anything.
//
// WHY A CLAIM AND NOT A LOG. Making the approval question survive a restart means a second producer:
// SweepApprovals reads GET /v1/approvals and asks about anything still open, which is precisely what a
// process that died between "a run parked" and "the buttons posted" leaves behind. But an approval stays
// open until somebody CLICKS it, so the sweep sees the ones the live arm has already asked about too — and
// would post them again, giving a reader two identical questions with two pairs of buttons and no way to
// tell which is real.
//
// THE ORDER IS CLAIM, POST, RELEASE-ON-FAILURE, and each part is load-bearing. Claiming first is what
// actually closes the double-post window: a record written after a successful post leaves both producers
// inside it. Releasing on failure is what keeps the claim from becoming the new silent loss — without it a
// single Slack hiccup would mark a question as asked forever and the run would park on a human who was
// shown nothing, which is the exact failure this whole change exists to make impossible. What survives
// therefore means "a human can see this question", never "we tried once".
type ApprovalPosts interface {
	// ClaimApprovalPost reserves approvalID. won=false means someone already has it — post nothing.
	ClaimApprovalPost(ctx context.Context, botID, approvalID, channelID, threadTS string) (bool, error)
	// ReleaseApprovalPost gives a claim back after a post that did not land.
	ReleaseApprovalPost(ctx context.Context, botID, approvalID string) error
}

func (deps ApprovalDeps) validate() error {
	switch {
	case deps.Palai == nil:
		return errors.New("relay: ApprovalDeps needs a Palai")
	case deps.Slack == nil:
		return errors.New("relay: ApprovalDeps needs a Slack")
	case deps.Posts == nil:
		return errors.New("relay: ApprovalDeps needs Posts — without the claim, a restart's recovery sweep asks every open question a second time")
	case deps.BotID == "":
		return errors.New("relay: ApprovalDeps needs a BotID — it is what the post claims are partitioned by")
	}
	return nil
}

// approvalListPageSize/maxApprovalListPages bound findPendingApproval's scan of GET /v1/approvals.
const (
	approvalListPageSize = 100
	maxApprovalListPages = 20
)

// ErrApprovalNotFound is findPendingApproval finding no open (pending) approval with the request hash
// an approval.requested.v1 event named — the row has not committed visibly yet, or (see OnButton's own
// 404/409 handling) it was already decided by the time this ran.
var ErrApprovalNotFound = errors.New("relay: no open approval matches this request hash")

// OnApprovalRequested turns one approval.requested.v1 event into the Slack message a human decides
// from. channel/threadTS are NOT read off ev — nothing in a Palai event names a Slack thread, that
// mapping is this bot's own (Task 8's ThreadStore, keyed the other direction: thread -> session, with
// no reverse query) — so the caller wiring this bridge into a run's event loop must supply them the
// same way relay.Run itself takes them as explicit parameters, not derive them here.
//
// THE EVENT DOES NOT CARRY WHAT THIS FUNCTION NEEDS TO RENDER A SCREEN, and the two kinds that share
// this event type do not even agree on what they DO carry — measured directly against the two
// journalling call sites:
//
//   - packages/coordinator/approvals.go RequestToolApproval's payload:
//     {tool_call_id, approval_id, tool_name, request_hash, run_id}
//   - packages/coordinator/publication.go RequestPublication's payload (line 269-272):
//     {publication_id, operation, branch, request_hash, display} — NO approval_id.
//
// request_hash is the one field both share, so it is the only thing this function reads off ev.data;
// everything else — the approval's own id, and the already-derived Identity/OperatorLabel/Arguments/
// Truncated GET /v1/approvals computes server-side (store/approvals.go) — comes from listing the
// tenant's open approvals and matching this hash (findPendingApproval).
func OnApprovalRequested(ctx context.Context, deps ApprovalDeps, channel, threadTS string, ev palai.Event) error {
	if err := deps.validate(); err != nil {
		return err
	}
	if ev.Type != ApprovalRequestedEventType {
		return fmt.Errorf("relay: OnApprovalRequested called with a %q event, not %q", ev.Type, ApprovalRequestedEventType)
	}
	requestHash, _ := ev.Data["request_hash"].(string)
	if requestHash == "" {
		return fmt.Errorf("relay: %s event for session %s carries no request_hash", ev.Type, ev.SessionID)
	}

	approval, err := findPendingApproval(ctx, deps, requestHash)
	if err != nil {
		return err
	}

	_, err = postApprovalOnce(ctx, deps, channel, threadTS, approval)
	return err
}

// postApprovalOnce is the ONE place either producer puts an approval question into Slack — the live event
// arm above and SweepApprovals below both come through here, which is what makes the claim a real
// guarantee rather than a convention two call sites are each expected to honour.
//
// A LOST CLAIM IS A SUCCESS, not a refusal: it means the question is already on screen, which is the whole
// thing the caller wanted. Returning an error for it would make the sweep's ordinary, every-few-seconds
// outcome look like a failure in the log.
//
// BUT IT IS NOT A POST EITHER, and keeping those two apart is what `posted` is for. It was NOT here when
// this file was first written, and the omission was caught by the live proof rather than by any test: the
// sweep counted every lost claim as an asking, so a single parked approval made it log "asked about 1
// parked run(s)" on EVERY tick, forever, while Slack showed exactly one question. Nothing was
// double-posted — the claim held — but the LOG said a human had just been asked, once every fifteen
// seconds, about a question asked once. A line that reports an action nobody took is the same defect as
// an action nobody reports, and it is the one an operator would have chased.
func postApprovalOnce(ctx context.Context, deps ApprovalDeps, channel, threadTS string, approval palai.Approval) (posted bool, err error) {
	if approval.ID == "" {
		return false, fmt.Errorf("relay: an approval with no id cannot be posted exactly once (session %s)", approval.SessionID)
	}
	won, err := deps.Posts.ClaimApprovalPost(ctx, deps.BotID, approval.ID, channel, threadTS)
	if err != nil {
		return false, fmt.Errorf("relay: claim the right to ask about approval %s: %w", approval.ID, err)
	}
	if !won {
		return false, nil // already asked; a second pair of buttons would be worse than silence
	}
	if _, err := deps.Slack.PostMessage(ctx, buildApprovalMessage(channel, threadTS, approval)); err != nil {
		// THE CLAIM GOES BACK. Keeping it would turn one failed Slack call into a question nobody is ever
		// asked and a run parked forever — the failure this file exists to prevent, reintroduced by its own
		// de-duplication. If the release ALSO fails there is nothing further to try, and it is reported
		// alongside the post failure rather than replacing it.
		if relErr := deps.Posts.ReleaseApprovalPost(ctx, deps.BotID, approval.ID); relErr != nil {
			return false, fmt.Errorf("relay: post approval message for %s: %w (and the claim could not be released, so no later sweep will retry it: %v)", approval.ID, err, relErr)
		}
		return false, fmt.Errorf("relay: post approval message for %s: %w", approval.ID, err)
	}
	return true, nil
}

// ApprovalThreads resolves the Slack thread a session's approval belongs in. It is the reverse of the
// mapping the rest of this bot keeps: an approval row names a session_id and carries nothing Slack at all
// (GET /v1/approvals), so the sweep cannot know where to ask without it.
//
// found=false is the ordinary case rather than an error, and that is the point of the whole filter: this
// project's approvals include every run started from the console, the CLI or another bot, and none of
// those belong in any Slack thread. A sweep that posted them would take a question asked somewhere else
// and put it, with working buttons, in front of a Slack channel.
type ApprovalThreads interface {
	ThreadForSession(ctx context.Context, botID, sessionID string) (store.PendingRun, bool, error)
}

// SweepApprovalDeps is the sweep's seam: the bridge itself plus the thread lookup it needs and a place to
// say what it did.
type SweepApprovalDeps struct {
	Approvals ApprovalDeps
	Threads   ApprovalThreads
	Logf      func(string, ...any)
}

func (d SweepApprovalDeps) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// SweepApprovals asks about every open approval belonging to a thread this bot owns that nobody has been
// asked about yet.
//
// IT IS THE DURABLE HALF OF THE APPROVAL QUESTION, and the hole it fills was the quietest one this process
// had. A gated tool call parks its run server-side and journals approval.requested.v1; the live arm posts
// the buttons when it sees that event go by. If this process was not running at that instant — restarted,
// crashed, redeployed — nobody was ever asked, nothing timed out visibly, and the thread simply stopped.
// dispatch.go already named that outcome ("NOBODY WAS ASKED") because it was a known ceiling; this is the
// floor under it.
//
// IT RUNS ON A TIMER, NOT ONLY AT BOOT, and the extra cost is one listing call every interval against a
// route that returns an empty page in the ordinary case. What it buys is that the same code closes both
// holes: the restart, and a live process that missed the event because its stream dropped. A boot-only
// scan would leave the second one open and would look identical in every test.
//
// EVERY FAILURE IS PER-APPROVAL. One row that cannot be resolved or posted must not stop the sweep, or a
// single broken thread would starve every other pending question in the workspace; the errors are counted
// and reported, and the next tick tries again.
func SweepApprovals(ctx context.Context, deps SweepApprovalDeps) error {
	if err := deps.Approvals.validate(); err != nil {
		return err
	}
	if deps.Threads == nil {
		return errors.New("relay: SweepApprovalDeps needs Threads — an approval names a session, and only this bot knows which thread that is")
	}

	after := ""
	var asked, failed int
	for page := 0; page < maxApprovalListPages; page++ {
		result, err := deps.Approvals.Palai.ListApprovals(ctx, palai.ListApprovalsParams{After: after, Limit: approvalListPageSize})
		if err != nil {
			return fmt.Errorf("relay: list open approvals: %w", err)
		}
		for _, approval := range result.Data {
			if approval.SessionID == "" {
				continue
			}
			thread, found, err := deps.Threads.ThreadForSession(ctx, deps.Approvals.BotID, approval.SessionID)
			if err != nil {
				failed++
				deps.logf("relay: could not resolve the thread for approval %s (session %s): %v", approval.ID, approval.SessionID, err)
				continue
			}
			if !found {
				continue // not this bot's session: a console run, another bot, another channel entirely
			}
			posted, err := postApprovalOnce(ctx, deps.Approvals, thread.ChannelID, thread.ThreadTS, approval)
			if err != nil {
				failed++
				deps.logf("relay: NOBODY HAS BEEN ASKED about approval %s — its run is parked in %s/%s waiting for a decision and the question is not on screen: %v",
					approval.ID, thread.ChannelID, thread.ThreadTS, err)
				continue
			}
			// Counted only when this sweep actually put the question on screen — see postApprovalOnce for
			// the live-measured reason `posted` exists at all.
			if posted {
				asked++
			}
		}
		if !result.HasMore || result.NextCursor == nil {
			break
		}
		after = *result.NextCursor
	}
	// Only ever says something when something happened: this runs on a timer, and a line per tick would
	// bury the boot output an operator actually reads under an empty sweep every few seconds.
	if asked > 0 || failed > 0 {
		deps.logf("relay: approval sweep asked about %d parked run(s); %d could not be asked", asked, failed)
	}
	return nil
}

// findPendingApproval scans GET /v1/approvals (oldest first, per-page) for the row carrying
// requestHash. It is a scan rather than a filtered lookup because the route accepts no
// ?request_hash= filter (sdks/go/approvals.go's ListApprovalsParams doc: only after/limit/
// created_after/created_before are honoured) — this bridge is the first caller that has needed one.
func findPendingApproval(ctx context.Context, deps ApprovalDeps, requestHash string) (palai.Approval, error) {
	after := ""
	for page := 0; page < maxApprovalListPages; page++ {
		result, err := deps.Palai.ListApprovals(ctx, palai.ListApprovalsParams{After: after, Limit: approvalListPageSize})
		if err != nil {
			return palai.Approval{}, fmt.Errorf("relay: list approvals: %w", err)
		}
		for _, a := range result.Data {
			if a.RequestHash == requestHash {
				return a, nil
			}
		}
		if !result.HasMore || result.NextCursor == nil {
			break
		}
		after = *result.NextCursor
	}
	return palai.Approval{}, ErrApprovalNotFound
}

// buildApprovalMessage renders one approval row into a chat.postMessage body via
// slack.ToolApprovalMessage — see the file doc for why that function and not a hand-rolled one.
//
// EVERY STRING slack.ToolApprovalMessage PUTS ON THE SCREEN IS RE-DERIVED THROUGH
// slack.DeriveApprovalDisplay / slack.ApprovalArgumentRows HERE, and for a PUBLICATION approval that
// re-derivation is not redundant — it is the FIRST time these bytes are defused at all. GET
// /v1/approvals derives a TOOL row's Identity/OperatorLabel/Arguments through
// slack.DeriveApprovalDisplay server-side (store/approvals.go:64), but a PUBLICATION row's (line 90)
// is `Identity: p.Operation, OperatorLabel: p.Display, Arguments: string(p.Arguments)` — the raw
// columns, untouched by NeutralizeBroadcasts. p.Display is exactly the kind of string
// interactions.go's own UpdateMessage doc warns is run-controlled ("the decision repair names a
// publication display built from THE RUN'S OWN BRANCH NAME, and `<!channel>` is a valid git ref").
// Passing approval.Identity/OperatorLabel/Arguments through ToolApprovalMessage — which neutralizes
// all three again before rendering — closes that gap for the Slack surface this bridge owns. For a
// TOOL row it is a second, idempotent pass over already-defused text: NeutralizeBroadcasts only
// rewrites a literal `<!`/`<@`, and defused text (`&lt;!`/`&lt;@`) contains neither.
//
// The approve/deny/show-arguments buttons all carry the SAME value: approvalActionValue(approval.ID,
// approval.RequestHash), not the bare request hash slack.ApprovalRequest's own doc names — see
// OnButton's doc for why this bridge packs both ids into the one string a button carries, and why
// that is safe as a private convention between this function and OnButton alone.
func buildApprovalMessage(channel, threadTS string, approval palai.Approval) []byte {
	return slack.ToolApprovalMessage(channel, threadTS, slack.ApprovalRequest{
		ApprovalID:    approval.ID,
		RequestHash:   approvalActionValue(approval.ID, approval.RequestHash),
		Identity:      approval.Identity,
		OperatorLabel: approval.OperatorLabel,
		Arguments:     []byte(approval.Arguments),
	})
}

// approvalActionValue packs the two ids a decision needs into the one string a Block Kit button
// carries. Approvals.Approve/Deny (sdks/go/approvals.go) take the approval's own id as a URL path
// segment AND its request hash as a MANDATORY body field — DecisionParams.RequestHash, which the
// server refuses empty with 400 "an approval id alone authorizes nothing" (verified live against the
// running control plane). `|` is safe as a separator because neither half is Slack- or human-authored:
// both come straight off the GET /v1/approvals row findPendingApproval just read, never off anything
// a click could have carried in.
func approvalActionValue(approvalID, requestHash string) string {
	return approvalID + "|" + requestHash
}

// parseApprovalActionValue reverses approvalActionValue. ok is false for anything that is not exactly
// two non-empty halves — an empty approval id or request hash authorizes nothing, matching how
// slack.MapInteractiveApproval already refuses an empty button value for the same reason. Splitting
// on the FIRST `|` only (strings.Cut) is deliberate: approvalID is ours and never contains one, so
// whatever remains — even if a request hash somehow did — is still the whole second half.
func parseApprovalActionValue(value string) (approvalID, requestHash string, ok bool) {
	approvalID, requestHash, found := strings.Cut(value, "|")
	if !found || approvalID == "" || requestHash == "" {
		return "", "", false
	}
	return approvalID, requestHash, true
}

// ErrApproverNotAllowed is an unlisted Slack user's click. OnButton returns it before ever calling
// deps.Palai — see OnButton's own doc for why the ORDER, not just the outcome, is the property this
// file's test asserts.
var ErrApproverNotAllowed = errors.New("relay: this Slack user is not an allowed approver for this bot")

// OnButton turns one decoded, already-authenticated Slack approve/deny click into a decision.
//
// click is a slack.ApprovalIntent — MapInteractiveApproval's own output type, not a slack.Interaction
// (no such type exists; see the file doc). Its RequestHash field, on a click THIS bridge minted the
// button for, holds approvalActionValue's composite string rather than a bare hash — a different wire
// convention on the same shared field than the OLDER admitter-based Slack integration embedded in the
// control plane uses for the SAME struct. That is safe only because this bridge is also the only
// reader of its own buttons' values; it must never be assumed elsewhere in this package.
//
// THE ALLOW-LIST CHECK RUNS BEFORE deps.Palai IS EVER CALLED, and the ordering is load-bearing, not
// tidy (see ApprovalDeps's own doc on why AllowedApprovers is the only per-human boundary this path
// has): a version that called the API first and checked the allow-list on the way out would still let
// an unauthorized click reach the server on every attempt, race or not.
//
// A deny sends no Reason: this bridge decides from a channel-message button, never a modal, so there
// is no free-text field to lose in the first place. That sidesteps rather than papers over the known
// platform gap (HIL-P10, sdks/go/approvals.go's DecisionParams doc): a publication denial's Reason is
// dropped server-side (ApplyApprovalDecision, packages/coordinator/publication.go, takes no reason
// parameter at all) while a tool denial's reaches the model verbatim. Since this file never offers a
// reason box to type into, it can never show a human a box that silently swallows their words.
func OnButton(ctx context.Context, deps ApprovalDeps, click slack.ApprovalIntent) error {
	if err := deps.validate(); err != nil {
		return err
	}
	if !allowed(deps.AllowedApprovers, click.UserID) {
		return fmt.Errorf("relay: %w: %s", ErrApproverNotAllowed, click.UserID)
	}

	approvalID, requestHash, ok := parseApprovalActionValue(click.RequestHash)
	if !ok {
		return fmt.Errorf("relay: button value %q is not a bound approval/request-hash pair", click.RequestHash)
	}

	var result *palai.ApprovalDecisionResult
	var err error
	switch click.Decision {
	case "approve":
		result, err = deps.Palai.ApproveApproval(ctx, approvalID, palai.DecisionParams{RequestHash: requestHash})
	case "deny":
		result, err = deps.Palai.DenyApproval(ctx, approvalID, palai.DecisionParams{RequestHash: requestHash})
	default:
		return fmt.Errorf("relay: %q is not a decision this bridge makes", click.Decision)
	}

	if err != nil {
		if isAlreadyDecided(err) {
			// NEVER SURFACE "no such approval": a decided PUBLICATION 404s (PublicationApprovalByID
			// filters WHERE p.state = 'pending_approval', so a decided row falls out of the lookup
			// entirely) and a decided TOOL call 409s (ToolApprovalByID carries no state filter at
			// all) — two different codes for the SAME fact, that nothing is left for this clicker to
			// decide, most often because someone else's click landed first. Repair the message rather
			// than error the caller.
			return deps.Slack.UpdateMessage(ctx, slack.UpdateMessage(click.ChannelID, click.MessageTS,
				"This approval was already decided — refresh the thread to see by whom. No action was taken from this click.", ""))
		}
		return fmt.Errorf("relay: %s approval %s: %w", click.Decision, approvalID, err)
	}

	return deps.Slack.UpdateMessage(ctx, slack.UpdateMessage(click.ChannelID, click.MessageTS,
		fmt.Sprintf("Decision recorded: %s.", result.Decision), click.UserID))
}

// allowed reports whether userID is one of this bot's configured approvers: an exact,
// case-sensitive match against the Slack user id, never a substring or prefix match — the class of
// comparison this tree has shipped defeated more than once elsewhere (see path/membership-comparison
// history in this repo's plan discipline notes).
func allowed(list []string, userID string) bool {
	for _, id := range list {
		if id == userID {
			return true
		}
	}
	return false
}

// isAlreadyDecided reports whether err is either half of the split Approvals.Approve/Deny's own doc
// measures: 409 approval_not_decidable for a tool call already decided, or 404 for a publication
// already decided. Both collapse to one branch here because they mean the same thing to a human.
func isAlreadyDecided(err error) bool {
	var apiErr *palai.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusNotFound
}
