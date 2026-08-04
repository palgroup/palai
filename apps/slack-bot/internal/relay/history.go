package relay

import (
	"context"
	"time"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

// THE PICTURE THAT WAS POSTED ONE MESSAGE EARLIER.
//
// THE OBSERVATION THIS IS FOR, and it is the same one the control plane's own bridge was built for
// (apps/control-plane/internal/extensions/slack_thread.go, which this relay shares no code with and must not
// diverge from): a human drops a screenshot into a thread, and in the NEXT message writes "@Palai fix the bug
// in this". The run is born from a message carrying no attachment at all, so the bot answers that it cannot
// see any screenshot — about a screenshot sitting one line above it. The image leg (images.go) reads
// ev.Files, which is the TRIGGERING message's own files and nothing else, so before this file the picture was
// invisible however plainly it was there.
//
// THE FETCH RULE, and it is deliberately the narrowest one that closes that:
//
//		read  ⇔  the event is a REPLY IN A THREAD  ∧  this thread has no session yet  ∧  a leg can attach images
//
//	  - THE SECOND CLAUSE is inherited whole, and it is the one that keeps this from being expensive and wrong.
//	    A thread this bot already holds (HandleEvent's `correlated`) has its own Palai session, and every image
//	    shared in it while the bot was present was ALREADY attached to the turn it arrived with — the control
//	    plane replays those turns, images included, which is exactly why it caps them (execution.maxRunImages).
//	    Reading history there would re-fetch, re-upload and re-attach pictures the conversation already
//	    carries, spending that cap on duplicates. So a follow-up in a conversation the bot is already in reads
//	    NOTHING, ever.
//	  - THE FIRST CLAUSE is not an optimisation, it is correctness about cost: a top-level mention IS its own
//	    thread root, so conversations.replies would return that one message — the files this relay already has
//	    on ev — and nothing else. Slack's own thread_ts answers it (slack.Event.InThread), the same authority
//	    the run-birth rule uses.
//	  - THE THIRD CLAUSE is what stops a read whose result cannot be used: with no image leg mounted, or one
//	    missing a token or an artifact writer, nothing here can become something the model sees, so the call
//	    would be a Slack round trip paid for nothing.
//
// WHAT MAKES THIS READ SCOPED, restated at the point of use because the app holds `channels:history` and a
// read keyed on an id somebody else chose IS an arbitrary read of the workspace (the confused-deputy shape):
//
//  1. THE TARGET IS THE ADMITTED EVENT'S OWN COORDINATES — ev.ChannelID and ev.ThreadTS, both put on the
//     event by Slack, on a message this bot was addressed in (birthsRun has already said yes before this
//     runs). Nothing else is consulted: not ev.Context — what the human is LOOKING at, which is a different
//     thing entirely and which this relay resolves nowhere — not a payload field, not a stored id.
//  2. NOTHING A MODEL SAYS CAN REACH IT. This runs before the run exists, on coordinates the model never
//     sees, and no tool in the relay's surface can address Slack.
//  3. THE TOKEN NEVER LEAVES THE SEAM. It is closed over by the production ThreadHistory and rides one
//     Authorization header; it reaches no prompt, no log and no artifact.
//
// CEILING, NAMED: this relay has no allowed_channels of its own (the old bridge's ChannelAllowed has no
// counterpart here), so what bounds WHICH channels can be read is Slack's own invitation model — the bot
// reads a thread in a channel it was invited into and addressed in. That is the same bound its answers
// already have, but it is a bound of a different kind and it is worth knowing which one is doing the work.
//
// WHAT IS NOT BUILT HERE, on purpose: the thread's TEXT. The old bridge also renders the fetched messages as
// an untrusted prefix of the run's input (slackThreadNote, ~115 lines plus speaker-name resolution), so a bot
// invited into a discussion can summarise it. That is a separate capability with its own prompt-shaping
// rules; this file takes only the files, which is what the reported defect is about. The read it makes is the
// same one that half needs, so adding it later costs the rendering and not the fetch.
const (
	// threadHistoryMaxMessages bounds the page, and the number is chosen for ONE property rather than as a
	// size guess: Slack's conversations.replies returns "the earliest messages in the time range first", so a
	// page is the START of a thread, never its tail — and the picture this file exists to find is at the TAIL.
	// A page big enough to hold the WHOLE thread is therefore the only page whose newest messages are the
	// thread's newest messages, and threadImages refuses outright when Slack says the thread did not fit (see
	// its hasMore branch). 200 is well past any thread in this workspace and far under Slack's own 1000
	// ceiling; the transfer is bounded by the adapter's own 4 MiB read either way.
	threadHistoryMaxMessages = 200
	// threadHistoryBudget bounds the read. It is NOT an acknowledgement deadline — Socket Mode has already
	// acked the envelope by the time any of this runs (socket.workBudget's own comment) — it is a HUMAN
	// latency bound: this call sits on the path between the message being sent and the first thing appearing
	// in the thread, ahead of the image fetches, the artifact uploads and the response create that follow it.
	// A read that has not answered in this long costs the turn its earlier pictures rather than costing the
	// person another two seconds of an empty thread.
	threadHistoryBudget = 2 * time.Second
	// maxThreadImages caps how many images the EARLIER messages of a thread may contribute, and it is a
	// SECOND budget rather than a share of maxImagesPerMessage: the message the human actually sent must
	// never lose a slot to the thread's older pictures.
	//
	// The arithmetic that matters is against the platform's own per-request ceiling: 3 + 3 is 6, under
	// execution.maxRunImages (8), so a first turn cannot on its own exceed what one model request carries.
	// Past that ceiling the control plane does NOT drop the run — it keeps the most recent images and leaves
	// a marker naming each one it could not carry — which is why exceeding it later in a conversation is a
	// degradation the reader is told about rather than a failure.
	maxThreadImages = 3
)

// ThreadHistory is the one Slack read this file makes, narrowed to exactly it — the same "narrow to what
// this file needs" shape ThreadStore, EventStream and ArtifactCreator already take, so a test substitutes a
// fake with no HTTP round trip and no token anywhere near it.
//
// hasMore is carried through rather than dropped because it is load-bearing here and not decoration: it is
// Slack's own answer to "did this page hold the whole thread", and threadImages refuses to attach anything
// when it did not.
type ThreadHistory interface {
	ThreadReplies(ctx context.Context, channel, threadTS string, limit int) (msgs []slack.ThreadMessage, hasMore bool, err error)
}

// NewThreadHistory builds the production ThreadHistory over the shared adapter's conversations.replies call,
// mirroring NewChannelSlackStreamer/NewApprovalSlack: same Doer/apiBase/token shape, the token closed over so
// no caller above this line ever holds it.
func NewThreadHistory(doer slack.Doer, apiBase string, token []byte) ThreadHistory {
	return webAPIThreadHistory{doer: doer, apiBase: apiBase, token: token}
}

type webAPIThreadHistory struct {
	doer    slack.Doer
	apiBase string
	token   []byte
}

func (h webAPIThreadHistory) ThreadReplies(ctx context.Context, channel, threadTS string, limit int) ([]slack.ThreadMessage, bool, error) {
	return slack.ThreadReplies(ctx, h.doer, h.apiBase, h.token, channel, threadTS, limit)
}

// WithThreadHistory mounts the read above. Like WithImages it is a builder rather than another constructor
// parameter, for the reason that one gives: this half is genuinely optional, and a constructor that demanded
// it would force every test of the text path to pass a nil to say "as before".
//
// Without it a thread the bot is invited into late behaves exactly as it did before this file existed — the
// triggering message's own attachments and nothing more.
func (deps InboundDeps) WithThreadHistory(history ThreadHistory) InboundDeps {
	deps.History = history
	return deps
}

// earlierThreadImages reads the thread this event arrived in and returns the images its EARLIER messages
// shared, plus how many it left behind. It returns nothing for any reason at all: no thread history is a
// less useful answer, while a failed turn is a lost message. Every failure is a log line and an empty result.
func (deps InboundDeps) earlierThreadImages(ctx context.Context, ev slack.Event) (files []slack.SharedFile, dropped int) {
	if deps.History == nil || !deps.Images.Ready() {
		return nil, 0
	}
	fetchCtx, cancel := context.WithTimeout(ctx, threadHistoryBudget)
	defer cancel()
	msgs, hasMore, err := deps.History.ThreadReplies(fetchCtx, ev.ChannelID, ev.ThreadTS, threadHistoryMaxMessages)
	if err != nil {
		// `missing_scope` is the expected one and it is a POSTURE fact rather than a defect: this app is
		// granted channels:history and im:history, so a PRIVATE channel (groups:history) or a group DM
		// (mpim:history) answers exactly that. Logged with Slack's own code so an operator can tell it from a
		// thread that has since been deleted.
		if code := slack.APIErrorCode(err); code != "" {
			deps.logf("slack-bot: not reading the thread in %s for earlier images — Slack refused with %s; the turn is relayed without them", ev.ChannelID, code)
			return nil, 0
		}
		deps.logf("slack-bot: the thread read in %s failed: %v; the turn is relayed without its earlier images", ev.ChannelID, err)
		return nil, 0
	}
	if hasMore {
		// THE PAGE IS THE THREAD'S BEGINNING, NOT ITS END. conversations.replies returns the earliest
		// messages first, so a thread that did not fit gives us its OLDEST messages — and the picture this
		// read exists to find is the one posted moments ago. Attaching from such a page would hand the model
		// a screenshot from hundreds of messages back and call it "shared earlier in this thread", which is
		// worse than attaching nothing: it is confidently wrong rather than merely incomplete.
		deps.logf("slack-bot: the thread in %s is longer than the %d messages that were read, so its earlier images are not attached — one page holds a thread's OLDEST messages, never its most recent",
			ev.ChannelID, threadHistoryMaxMessages)
		return nil, 0
	}
	return threadImages(msgs, ev, maxThreadImages)
}

// threadImages picks which of a thread's shared files are worth fetching, and reports how many image-shaped
// files it left behind. It is a pure function so the three decisions in it are testable without a fetch.
//
//   - THE TRIGGERING MESSAGE IS SKIPPED BY ITS ID (ev.MessageTS), never by a text heuristic: conversations.
//     replies returns the message that caused this read, and taking it here would fetch, upload and attach
//     one screenshot TWICE — once as ev.Files and once as "earlier context". The file ids are deduplicated
//     as well, which covers the same picture RE-SHARED into the thread (one Slack file object, many shares).
//   - THE NEWEST MESSAGES ARE READ FIRST, because the budget is small and the picture a request is about is
//     the one posted nearest to it. A forward walk would spend all three slots on a thread's oldest images
//     and drop the screenshot from one message ago — the exact case this file exists for.
//   - THE RESULT IS CHRONOLOGICAL even though the selection is not, and that ordering is load-bearing twice
//     over: it is how a reader would have seen them, and the control plane spends its own per-request image
//     budget on the LAST images in the input (execution.decodeMessages), so putting the thread's older
//     pictures ahead of the message's own is what makes the human's own attachment the one that survives
//     when a long conversation runs past that ceiling.
func threadImages(msgs []slack.ThreadMessage, ev slack.Event, limit int) (files []slack.SharedFile, dropped int) {
	seen := make(map[string]bool, len(ev.Files))
	for _, f := range ev.Files {
		seen[f.ID] = true
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].TS == ev.MessageTS {
			continue
		}
		// max is len(msgs[i].Files) so ImageCandidates disqualifies (a PDF, a 50 MiB "screenshot") without
		// capping. Its `skipped` is DISCARDED here, and that asymmetry with the triggering message's own
		// files is deliberate: a document somebody shared in this thread hours ago was never offered to this
		// request, so counting it into the prompt as something that "could not be attached" would report a
		// refusal nobody asked for. What IS counted is an IMAGE this read found and could not carry.
		candidates, _ := slack.ImageCandidates(msgs[i].Files, len(msgs[i].Files), maxImageBytes)
		var group []slack.SharedFile
		for _, f := range candidates {
			if seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			if len(files)+len(group) >= limit {
				dropped++
				continue
			}
			group = append(group, f)
		}
		// Prepended, so walking the messages newest-first still yields them oldest-first.
		files = append(group, files...)
	}
	return files, dropped
}
