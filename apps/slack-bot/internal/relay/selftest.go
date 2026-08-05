// The live self-test (2026-08-03 plan, Task 13): the last step of the flow an operator walks — choose the
// channel, take the manifest, install the app, paste the tokens back, and then ask the one question the
// console cannot answer from a row, "does it work?".
//
// FOUR LEGS, IN ORDER, AND THE FIRST FAILURE STOPS. Each leg is a different repair on a different screen,
// which is the whole reason they are separate results rather than one boolean:
//
//  1. auth.test          the bot token is valid, and names the workspace this row claims
//  2. Socket Mode        an app-level token opens a connection, and it closes cleanly
//  3. chat.postMessage   the app is in the channel and can speak in it
//  4. the stream trio    startStream → append → append → stopStream, read back
//
// A LEG IS NEVER RENAMED AND NEITHER IS ITS ERROR. Slack's own code rides into Detail verbatim
// (`invalid_auth`, `missing_scope`, `not_in_channel`, `channel_not_found`), because those are the words the
// operator will search for and the words that decide which screen they open. A message this package
// invented would send them here instead — and this file is the one place that cannot fix their token.
//
// WHAT PASSING DOES NOT PROVE, stated here rather than left for a report nobody reads beside the code: all
// four legs are properties of the CREDENTIALS and the channel. None of them proves a relay process is
// running, that a mention reaches it, or that a run answers — a bot row with no agent_revision_id passes
// every leg below and still cannot say a word to a human. The self-test answers "are these credentials able
// to do the things a relay needs to do", which is the question an operator who has just pasted three tokens
// is actually asking.
package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

// The exact sequence leg 4 drives. They are the plan's own strings, and two properties ride on them being
// these rather than anything shorter: each carries its OWN trailing space, so a lost separator is visible
// in the read-back; and they are non-ASCII, so the round trip proves the encoding survives Slack as well as
// the concatenation does.
const (
	selfTestPieceOne   = "parça bir "
	selfTestPieceTwo   = "parça iki "
	selfTestPieceClose = "kalan üç"
)

// selfTestOpening is what leg 4's chat.startStream opens the message with. It is not decoration: the
// measurement below asserts the FULL expected text, opening included, so text lost by the opening call is
// caught by the same comparison that catches text lost by an append.
const selfTestOpening = "Palai self-test: "

// StopStreamSemantics is what leg 4 OBSERVED chat.stopStream do with its `markdown_text` — a measurement
// with a date (see Report.StopStream), never an assumption.
//
// WHY IT IS RECORDED AT ALL. The relay's design rests on this: openStream.pending (relay.go) carries only
// text that has NOT been confirmed delivered, and it is flushed as the final stopStream's markdown_text. If
// stopStream APPENDS, that is a last attempt at the residue and everything already streamed stands. If it
// REPLACES, the closing call erases text that earlier appends delivered successfully — and the failure is
// not a duplicate an operator would notice, it is SILENT LOSS. So this is not a curiosity: it is the
// premise the relay would have to be redesigned around if Slack ever changed it.
type StopStreamSemantics string

const (
	// StopStreamAppends: the final message is everything sent, in order. What relay.go assumes.
	StopStreamAppends StopStreamSemantics = "appends"
	// StopStreamReplaces: the final message is the closing text alone — earlier appends are gone.
	StopStreamReplaces StopStreamSemantics = "replaces"
	// StopStreamUnrecognised: the message came back as neither. It is a THIRD outcome rather than a
	// failure to observe, because "Slack did something else with it" is exactly as design-relevant as
	// replace and would be hidden by folding it into either of the other two.
	StopStreamUnrecognised StopStreamSemantics = "neither — see Streamed"
)

// SelfTestSlack is the three legs that are single calls. The stream trio is NOT here: it is the `Slack`
// seam relay.Run itself drives (relay.go), reached through SelfTestDeps.NewStream, so leg 4 exercises the
// same surface production streams through rather than a second one written for a test.
type SelfTestSlack interface {
	// AuthTest is leg 1. The Identity it returns is used by leg 4: chat.startStream requires a recipient,
	// and a self-test has no human to be one — so the app is its own, which is a value only Slack can give.
	AuthTest(ctx context.Context) (slack.Identity, error)
	// OpenSocket is leg 2: one connection, opened and closed. The count is Slack's own num_connections.
	OpenSocket(ctx context.Context) (numConnections int, err error)
	// PostMessage is leg 3, and its ts is leg 4's thread parent.
	PostMessage(ctx context.Context, channel, text string) (ts string, err error)
	// MessageText reads ONE message back — leg 4's whole answer, since the semantics question can only be
	// settled by what the workspace ended up holding.
	MessageText(ctx context.Context, channel, threadTS, messageTS string) (string, error)
}

// SelfTestDeps is the self-test's whole seam.
type SelfTestDeps struct {
	Slack SelfTestSlack
	// NewStream opens a relay.Slack for one recipient — the SAME constructor production wires
	// (NewChannelSlackStreamer, inbound.go). Leg 4 fills the recipient from leg 1's Identity.
	NewStream func(recipientUserID, recipientTeamID string) Slack
	// Channel is where the test message goes. There is no default and there is no fallback: a self-test
	// that guessed a channel would post into somebody's workspace at a place nobody chose.
	Channel string
}

// Report is one run of the self-test. Every leg carries its own result because every leg is a different
// repair, and Detail names the FIRST failure — a report with three green legs and a red one has said
// exactly where to look.
type Report struct {
	AuthOK   bool
	SocketOK bool
	PostOK   bool
	StreamOK bool
	Detail   string

	// Identity is what leg 1 learned: which workspace these credentials actually belong to, and the `U…`
	// the app speaks as. A token from the wrong app is a VALID token — nothing but this names it.
	Identity slack.Identity
	// Connections is Slack's own num_connections at leg 2, this probe included. 2 or more means something
	// else — a running relay — is connected as this app too.
	Connections int
	// MessageTS is leg 3's message: the thread leg 4 streamed into, so an operator can go and look at it.
	MessageTS string
	// StopStream is leg 4's measurement, and Streamed is the raw text it was read from. Streamed is kept
	// even on success: it is the evidence, and a claim about Slack's behaviour with no transcript behind it
	// is the thing this tree keeps having to re-measure.
	StopStream StopStreamSemantics
	Streamed   string
}

// OK reports whether every leg passed. It exists because four bools are four chances to check three.
func (r Report) OK() bool { return r.AuthOK && r.SocketOK && r.PostOK && r.StreamOK }

// SelfTestUnproven is what a PASSING run has not shown, and it lives here — beside Report, exported, one
// string — rather than inside whichever renderer happens to print the result.
//
// The reason is that the sentence is easiest to forget exactly where it matters most. A failing report
// sends an operator somewhere; a PASSING one is the report that gets read as "my bot works", and every
// renderer of a green Report is therefore one place that can quietly drop the limit. Owning the words next
// to the type means a second renderer has to go out of its way to omit them instead of merely not thinking
// of them.
//
// It is not a hedge. The four legs really do settle four things, and this names them before it names what
// they leave open: a caveat that only subtracts reads as a disclaimer and gets skipped.
const SelfTestUnproven = "WHAT THIS DID NOT CHECK: that a relay process is running, that an @mention reaches it, or that a run answers. " +
	"All four legs are properties of the CREDENTIALS and the CHANNEL — the run pipeline is untouched by every one of them, " +
	"and a bot with no agent_revision_id passes all four and still cannot say a word to a person."

// SelfTest drives the four legs against a real workspace and reports what each one did.
//
// THE ERROR RETURN IS NOT A LEG FAILURE. It is returned only for a seam this package was handed
// incomplete — no Slack, no channel — because that is a caller's bug and not an answer about the
// workspace. Every failure the workspace itself gives is a Report with a false leg and a Detail, which is
// what "each leg reports its own result" has to mean for a caller that renders rather than branches.
func SelfTest(ctx context.Context, deps SelfTestDeps) (Report, error) {
	switch {
	case deps.Slack == nil:
		return Report{}, errors.New("relay: SelfTestDeps needs a Slack")
	case deps.NewStream == nil:
		return Report{}, errors.New("relay: SelfTestDeps needs NewStream — leg 4 drives the same stream seam the relay does")
	case deps.Channel == "":
		return Report{}, errors.New("relay: SelfTestDeps needs a Channel — a self-test does not guess where to post")
	}

	var r Report

	identity, err := deps.Slack.AuthTest(ctx)
	if err != nil {
		r.Detail = legFailure(1, "auth.test", err) +
			" — the bot token (config key bot_token_ref) is what this call presents; re-seal it in the admin panel (Bots → this bot → Credentials)"
		return r, nil
	}
	r.AuthOK = true
	r.Identity = identity

	connections, err := deps.Slack.OpenSocket(ctx)
	if err != nil {
		r.Detail = legFailure(2, "Socket Mode", err) +
			" — the app-level token (config key app_token_ref) is Socket Mode's only authentication, and it is a DIFFERENT token from the one leg 1 just accepted: xapp-, from App → Basic Information → App-Level Tokens, with connections:write"
		return r, nil
	}
	r.SocketOK = true
	r.Connections = connections

	messageTS, err := deps.Slack.PostMessage(ctx, deps.Channel, selfTestPostText(identity))
	if err != nil {
		r.Detail = legFailure(3, "chat.postMessage", err) +
			" — the token is good (leg 1 passed), so this is about the CHANNEL: `not_in_channel` means invite the app to " +
			deps.Channel + ", `channel_not_found` means it is not a channel this app can see"
		return r, nil
	}
	r.PostOK = true
	r.MessageTS = messageTS

	streamed, semantics, err := selfTestStream(ctx, deps, identity, messageTS)
	r.Streamed = streamed
	r.StopStream = semantics
	if err != nil {
		r.Detail = legFailure(4, "the stream trio", err)
		return r, nil
	}
	r.StreamOK = true
	r.Detail = fmt.Sprintf(
		"all four legs passed. This app is %s (%s) in workspace %s (%s, %s); Slack reported %d Socket Mode connection(s) open including this probe; the test posted in %s at ts %s; and chat.stopStream %s its markdown_text.",
		identity.User, identity.UserID, identity.Team, identity.TeamID, identity.URL,
		connections, deps.Channel, messageTS, semantics)
	return r, nil
}

// selfTestStream drives leg 4 and reads the result back. It returns the raw final text alongside the
// verdict, so a caller can print what was actually there when the verdict is not "appends".
//
// A FAILURE FROM ANY OF THE FOUR CALLS IS A FAILURE OF THE LEG, and the semantics stay unobserved rather
// than defaulting: "stopStream appends" is a claim, and a claim nothing measured is exactly what this leg
// exists to stop being made.
func selfTestStream(ctx context.Context, deps SelfTestDeps, identity slack.Identity, threadTS string) (string, StopStreamSemantics, error) {
	stream := deps.NewStream(identity.UserID, identity.TeamID)

	ts, err := stream.StartStream(ctx, deps.Channel, threadTS, selfTestOpening)
	if err != nil {
		return "", "", fmt.Errorf("chat.startStream: %w", err)
	}
	if err := stream.AppendStream(ctx, deps.Channel, ts, selfTestPieceOne); err != nil {
		return "", "", fmt.Errorf("the first chat.appendStream: %w", err)
	}
	if err := stream.AppendStream(ctx, deps.Channel, ts, selfTestPieceTwo); err != nil {
		return "", "", fmt.Errorf("the second chat.appendStream: %w", err)
	}
	if err := stream.StopStream(ctx, deps.Channel, ts, selfTestPieceClose); err != nil {
		return "", "", fmt.Errorf("chat.stopStream: %w", err)
	}

	// READ BACK FROM THE WORKSPACE, never from what was sent. The whole question is what Slack KEPT.
	final, err := deps.Slack.MessageText(ctx, deps.Channel, threadTS, ts)
	if err != nil {
		return "", "", fmt.Errorf("reading the streamed message back: %w", err)
	}

	semantics := classifyStopStream(final)
	if semantics != StopStreamAppends {
		return final, semantics, fmt.Errorf(
			"chat.stopStream %s its markdown_text. The relay flushes openStream.pending — text that earlier appends did NOT confirm — as the closing markdown_text, so anything but an append means text that streamed successfully is ERASED at close: silent loss, not a duplicate. Expected %q, Slack kept %q",
			semantics, selfTestOpening+selfTestPieceOne+selfTestPieceTwo+selfTestPieceClose, final)
	}
	return final, semantics, nil
}

// classifyStopStream decides which semantics the read-back shows.
//
// IT COMPARES THE WHOLE STRING AND NOT A PREFIX OR A SUBSTRING. This tree records defeated
// path/membership comparisons as a repeating defect, and the same shape bites here in an unusually quiet
// way: `strings.Contains(final, selfTestPieceClose)` is TRUE under replace semantics as well as append, so
// a substring test would report "appends" on precisely the workspace where the relay is losing text.
//
// The comparison is on TrimSpace'd values because Slack's own read-back trims the message's outer
// whitespace, and the closing piece ends without one — an equality that failed on that would report
// "neither" for a workspace that appends perfectly.
func classifyStopStream(final string) StopStreamSemantics {
	got := strings.TrimSpace(final)
	switch got {
	case strings.TrimSpace(selfTestOpening + selfTestPieceOne + selfTestPieceTwo + selfTestPieceClose):
		return StopStreamAppends
	case strings.TrimSpace(selfTestPieceClose):
		return StopStreamReplaces
	}
	return StopStreamUnrecognised
}

// selfTestPostText is leg 3's message. It says what it is and who ran it, because it lands in a channel
// real people read and an unexplained bot message is a support question.
func selfTestPostText(identity slack.Identity) string {
	return fmt.Sprintf("Palai self-test — checking that %s can post here. The reply below streams in four parts.", identity.User)
}

// legFailure is the one sentence a failing leg produces: which leg, by number and by the call it makes,
// and then SLACK'S OWN words. The number is there because an operator reading a support thread quotes it.
func legFailure(n int, name string, err error) string {
	if code := slack.APIErrorCode(err); code != "" {
		return fmt.Sprintf("leg %d of 4 (%s) failed: %s", n, name, code)
	}
	return fmt.Sprintf("leg %d of 4 (%s) failed: %v", n, name, err)
}
