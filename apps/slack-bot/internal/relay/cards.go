package relay

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// WHAT A READER GETS TO SEE, and why this file exists at all.
//
// THE COMPLAINT, in the operator's own words on 2026-08-04: "thinking blokları gelmiyor sürekli dönüyor tool
// call oluyor tool call'un da detayı da yok ne okudu ne etti". Read back off their own thread with
// conversations.replies (channel C0B8BSXETHV, message ts 1785862748.518249), what they were looking at was:
//
//	[PLAN] title="Something went wrong"
//	   - error    | "Thinking"           | details=None
//	   - complete | "Running a command"  | details=None      ← ×29 more, byte-identical
//	   - complete | "Reading files"      | details=None      ← ×3
//
// Thirty-three rows, two distinct strings between them, no detail on any of them, and a container headline
// that read one unchanging word for the six minutes and twenty-four seconds the run took. The message body
// was EMPTY — the run failed (resp_92415bf18d7dab546f5bb2d219259bf8, run.failed.v1 at 17:05:32) and the
// thread was never told so in words.
//
// THREE THINGS WERE MISSING AND THEY ARE THREE DIFFERENT FIXES:
//
//  1. The headline was Slack's, not ours. `plan_update` (blocks.PlanUpdateChunk) takes it back, so the one
//     line that is always visible says what is happening NOW instead of "Thinking" forever.
//  2. The steps had nothing to say because the journal had nothing to give them. It does now:
//     `tool_call.executing.v1` carries `arguments_summary` (commit 9172fea9), so a step can read
//     `$ grep -rln "Date of birth" repo` instead of "Running a command".
//  3. Thirty-three rows is a log, not a card. Consecutive calls to the SAME tool now share ONE card and list
//     themselves inside it.

// captionPrefix turns a tool's `arguments_summary` into the line a human reads.
//
// THE SUMMARY IS OPAQUE TO THIS PACKAGE ON PURPOSE — it is one line composed by toolbroker.ArgumentSummary
// from that tool's own argument shape, and re-parsing it here would put the same per-tool knowledge in two
// places. What this map holds is only what the summary CANNOT say about itself: whether it needs a word in
// front of it to be unambiguous.
//
//   - shell's summary IS a command line, so it takes a prompt glyph and nothing else: `$ swift build`.
//   - the file tool's and the text editor's summaries already lead with their verb (`read repo/App.swift`,
//     `str_replace repo/App.swift`), so a prefix would only repeat it.
//   - a commit's summary is a subject line and a pull request's is a title; either alone reads as prose with
//     no hint of what happened to it, so both are labelled.
//
// AN UNLISTED TOOL IS NOT A GAP. The five keys here mirror the five summarisers that exist upstream today,
// and the fallback (stepCaption) is what makes that mirroring safe rather than a coupling: a tool given a
// summariser tomorrow and no entry here renders as `<human phrase>: <summary>`, which is always readable.
// So drift costs a slightly longer line, never a wrong or missing one.
var captionPrefix = map[string]string{
	"palai.workspace.shell":       "$ ",
	"palai.workspace.file":        "",
	"str_replace_based_edit_tool": "",
	"palai.workspace.commit":      "Commit: ",
	"palai.publish.pull_request":  "Pull request: ",
}

// stepCaption renders ONE tool call as the line that says what it did, or "" when the journal gave nothing
// to say.
//
// EMPTY IS A REAL AND EXPECTED ANSWER, not a failure to handle: `arguments_summary` is optional by contract
// and is deliberately withheld from MCP and remote-HTTP tools, whose argument schemas a third party writes
// and may grow an `api_key` property tomorrow (packages/tool-broker/summary.go). A call with no summary
// therefore has nothing to caption, and everything below treats "" as "this step contributes no line" rather
// than substituting a placeholder — a placeholder repeating the tool's phrase is precisely the row the
// operator could not read.
func stepCaption(data map[string]any) string {
	summary := dataString(data, "arguments_summary")
	if summary == "" {
		return ""
	}
	if prefix, ok := captionPrefix[toolName(data)]; ok {
		return prefix + summary
	}
	return toolTitle(data) + ": " + summary
}

// maxCardLines bounds how many captions ONE card lists.
//
// IT IS OUR NUMBER AND THE CUT IS NEVER SILENT — past it the card writes cardTruncated once and stops, while
// the title's own ×N keeps counting, so a reader can see that twenty lines are standing in for thirty-three.
//
// The ceiling is ours because Slack's is not knowable: the reference states a 256-character limit on a
// task_update chunk and the live API does not enforce it (measured 2026-08-04 — a single 300-character
// `details`, a 300-character `title` and an ACCUMULATED 500 characters were all accepted and stored intact).
// A limit that is documented but not applied is the worst kind to lean on, so this bounds our own writes:
// twenty captions of at most 256 bytes each is a card that cannot grow past a few kilobytes however long the
// run gets.
const maxCardLines = 20

// cardTruncated is the one line that says the list stopped. It ends the details for good.
const cardTruncated = "…(later steps of this kind are not listed — the count above is the true total)"

// maxCardDetails is the ceiling on ONE card's ACCUMULATED `details`, counted in the bytes that reach
// Slack — and it is enforced here because SLACK DOES NOT ENFORCE IT. It accepts the write, answers `ok`,
// and stores nothing at all.
//
// MEASURED 2026-08-05 against the live API (workspace T0AMPM5JX8U), one card per message so that a
// failure names the FIELD rather than the message — the first sweep put five oversized cards in one
// message, lost all five including a size that had stored intact moments before, and was measuring the
// message the whole time:
//
//	sent 12000 in one chunk               -> ok, stored 12000
//	sent 12001 in one chunk               -> ok, stored 0        <- the WHOLE field, not the overflow
//	sent 6 × 2000 = 12000 accumulated     -> ok, stored 12000    <- so the limit is on the ACCUMULATION
//	sent 12 × 2000 = 24000 accumulated    -> ok, stored 0
//	sent 12000 bytes of `\*` (6000 shown) -> ok, stored 6000
//	sent 24000 bytes of `\*` (12000 shown)-> ok, stored 0        <- so it counts what is SENT
//
// THREE THINGS FOLLOW AND EACH ONE IS LOAD-BEARING BELOW.
//
//  1. The cut has to be OURS. An overflow is not a tail that gets trimmed — it is every line the card
//     ever wrote going missing, on a call that answered ok, so a card that walks over this line does not
//     lose its last paragraph, it loses the whole transcript and says nothing about it.
//  2. The count is taken AFTER the escape (see wireBytes). The last two rows are what settle that: the
//     same displayed text passes at one length and vanishes at another purely because of what the escape
//     added, so counting the string this package holds would be counting the wrong string.
//  3. It is not a thinking-specific guard. A tool progress notification carries up to 4 KiB
//     (coordinator.maxProgressMessage) and maxCardLines allows twenty of them, so the tool path could
//     already reach 80 KB and silently blank its own card; nothing had ever measured the wall it was
//     walking towards.
//
// It equals slack.MaxMarkdownText, and that is corroboration rather than coincidence: 12,000 characters
// is the streaming surface's own documented budget for `markdown_text`, and `details` is measured to
// share it.
const maxCardDetails = 12000

// cardDetailBudget is what a card may actually spend, holding back enough room for whichever truncation
// note it might have to end with. Taking the LONGER of the two notes is what lets either one be written
// unconditionally once the budget is exhausted.
//
// THE `+1` IS THE SEPARATOR AND IT WAS MISSING. A note written into details that already hold something
// carries a leading newline (cut), so the reserve is the note plus that byte — and the first version
// here reserved 82 bytes for a note that costs 83. TestTheTruncationNotesFitTheReserve is what found it,
// which is the whole reason a constant computed from two other constants still gets a test: the arithmetic
// is invisible, the failure it produces is a card that silently stores NOTHING, and the note that was
// supposed to make the cut visible is what would have pushed it over.
var cardDetailBudget = maxCardDetails - 1 - max(wireBytes(cardTruncated), wireBytes(thinkingTruncated))

// wireBytes is what `text` costs against maxCardDetails: its length AFTER the two transformations
// TaskUpdateChunk applies on the way out (blocks.go — NeutralizeBroadcasts, then EscapeCardDetails).
//
// IT CALLS THE REAL RENDERERS RATHER THAN MODELLING THEM. Both rules belong to the adapter, both have
// been changed there before, and a second copy of "which characters grow" living in this file would be a
// copy that drifts — measured wrong in one direction it truncates text that would have fitted, and in
// the other it lets a card blank itself.
func wireBytes(text string) int {
	return len(slack.EscapeCardDetails(slack.NeutralizeBroadcasts(text)))
}

// clipToWire returns the longest rune-aligned prefix of text costing at most budget wire bytes.
//
// THE SECOND LOOP IS NOT BELT-AND-BRACES. The per-rune sum in the first loop can UNDERSHOOT the real
// cost, because NeutralizeBroadcasts is the one transformation here that is not per-character: it
// rewrites the two-rune sequences `<!` and `<@` as five bytes, which is more than those two runes cost
// apart. So the prefix is verified against the real function and shortened until it genuinely fits —
// a loop that iterates at all only for text carrying a broadcast.
func clipToWire(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	end, spent := 0, 0
	for i, r := range text {
		cost := wireBytes(string(r))
		if spent+cost > budget {
			break
		}
		spent += cost
		end = i + utf8.RuneLen(r)
	}
	for end > 0 && wireBytes(text[:end]) > budget {
		_, size := utf8.DecodeLastRuneInString(text[:end])
		end -= size
	}
	return text[:end]
}

// THE MODEL'S REASONING, and the three decisions that put it where it is.
//
// THE COMPLAINT: "ai thinking blokları da görmem lazım ama o blokları da görmüyorum ki" (2026-08-05).
// The journal had begun carrying the model's own working as `model_step.thinking.v1` and this relay was
// skipping it as an unknown type, so the one thing a reader waits through most of a run for was the one
// thing the card never said.
//
//  1. IT IS A CARD, ONE PER MODEL STEP, keyed by the step's `model_request_id`. Not the headline: the
//     headline is REPLACED by whatever happens next, so reasoning routed there would be visible for the
//     seconds between one step and its tool call and then gone for good, and the run's story would end
//     with no record that the model had reasoned at all. Not the body: see thinkingLeaksIntoTheAnswer.
//  2. IT DOES NOT BREAK A TOOL GROUP. Consecutive calls to one tool share a card (see stepCard), and the
//     measured runs alternate step/tool/step/tool throughout — so a reasoning card that advanced
//     openStreamCards.current would split every burst back into a row per call and undo the grouping in
//     exactly the runs that need it most. think() never touches `current`, and never counts a step
//     either: "Done · 33 steps" counts the work, not the thinking about it.
//  3. EVERY WINDOW IS SHOWN, not the last one and not a summary, bounded by the card's own byte budget.
//     Showing only the newest would show a sentence fragment starting mid-clause — see addThinking for
//     the measurement that makes that not merely worse but incoherent. The volume this costs is small
//     and is itself measured: a whole step's reasoning was 505 bytes across three windows here
//     (2026-08-05), and 348 and 613 bytes in the coordinator's own measurements, because the provider
//     returns a SUMMARY of the reasoning rather than the raw chain. maxCardDetails is roughly twenty
//     times the largest of those, so the cut is for a pathological step and not for an ordinary one.
const (
	// thinkingCardMark opens the title and the headline, because a reasoning card sits in the same list
	// as "$ swift build" and a reader has to be able to tell an act from a thought at a glance. It is a
	// literal emoji rather than a `:thought_balloon:` colon-code: a task title is PLAIN TEXT (measured
	// 2026-08-04 — backticks and asterisks come back byte-identical), so nothing would expand the code,
	// and the emoji itself was measured to store byte-identically in a title on 2026-08-05.
	thinkingCardMark = "💭 "
	// thinkingCardFallback titles a reasoning card whose lead line is empty. It cannot happen through
	// think() (a window with no text creates no card) and exists so that a card can never be titled with
	// the empty string, which Slack rejects.
	thinkingCardFallback = "Thinking"
	// thinkingTruncated is the note that says a step's reasoning stopped being shown. It is written into
	// the card rather than dropped for the reason the coordinator gives its own ceiling the same
	// treatment: an answer has a second authoritative copy in the model step's terminal event and
	// REASONING HAS NONE, so a reader who cannot see the cut cannot tell a model that stopped reasoning
	// from a card that stopped recording it.
	thinkingTruncated = "…(the rest of this step's reasoning is not shown — the card's own ceiling)"
)

// thinkingCardTitle renders a reasoning card's headline from its lead line.
//
// IT IS TRIMMED TO ONE LINE, by the same function the container headline uses, and the number came off a
// live run rather than out of the air: growLead closes at the first newline OR at maxHeadline runes, and
// because it closes on a WINDOW boundary it overshoots — the run on 2026-08-05 produced a 128-rune lead
// from two windows of 63 and 65. A 128-character title is not a title; it is the paragraph, in the field
// that is supposed to label it.
//
// NOTHING IS LOST TO THE TRIM, which is what makes it safe: the whole of that lead is the FIRST thing in
// the card's details, immediately below. So the title labels and the details carry — and the title now
// says exactly what the container headline says, which is the same card described in the same words in
// the two places a reader meets it.
func thinkingCardTitle(lead string) string {
	if lead == "" {
		return thinkingCardMark + thinkingCardFallback
	}
	return trimHeadline(thinkingCardMark + lead)
}

// stepCard is ONE card in the plan: a run of CONSECUTIVE calls to the same tool.
//
// WHY CONSECUTIVE AND NOT PER CALL. The operator's run made thirty-three calls and drew thirty-three rows;
// grouping the runs of one tool takes that to about ten without hiding anything, because every caption still
// appears — in the card's details instead of in its own row. Slack's own `dense` mode is documented to
// "collapse consecutive tool calls into a single summarized task card", so this is the vendor's design
// intent; it measured as a no-op on the live API (identical bytes to `timeline`), so the collapsing is done
// here instead.
//
// WHY NOT ACROSS THE WHOLE RUN. Grouping every shell call in a run into one card would give the shortest
// list of all — two rows — and it would also reorder the story: a reader could no longer see that the bot
// read a file BETWEEN two commands. A consecutive group keeps the order of the run intact.
//
// A model step does NOT break a group. The model deciding what to run next is what happens between two calls
// of a burst, so breaking on it would split one burst into a row per call and undo the grouping in exactly
// the runs that need it most (the measured one alternates step/tool/step/tool throughout).
type stepCard struct {
	// id is the FIRST call's tool_call_id, not something this package mints. Slack advances a card when a
	// later chunk repeats its id, so the id has to be stable for the life of the group; a ledger row's
	// primary key is stable by construction and is already unique across the run.
	id string
	// tool is the registered tool name every member of this group shares — the thing that decides whether
	// the next call joins or starts a new card.
	tool string
	// phrase is the human title for that tool (toolTitles), held so the card can be re-titled without the
	// event that renames it having to carry a tool_name (a progress notification does not).
	phrase string

	// thinking marks a card that renders the model's REASONING for one model step instead of a group of
	// tool calls. It is a field on this type rather than a second type because everything that decides a
	// card's FATE is shared — the status pill, the run terminal that resolves whatever is still open, the
	// details budget — and only the title and the way text is appended differ. A separate type would have
	// had to be reached through an interface by openStreamCards.all, and a run terminal that forgot to
	// resolve one kind is exactly the spinning card SLK-P2 is about.
	thinking bool
	// thinkingLead is the opening LINE of this step's reasoning: the card's title and the container
	// headline while the step is running. See openStreamCards.think for why it is grown rather than taken
	// whole from the first window.
	thinkingLead string
	// leadClosed records that thinkingLead has met its newline (or filled a headline) and will not grow
	// again — which is also what stops the headline being rewritten once per window.
	leadClosed bool

	calls int
	// first is the opening call's caption, kept because it lives in the TITLE while the group has one member
	// and has to move down into the list when a second one arrives.
	first string
	// lines counts captions already written into details, against maxCardLines.
	lines int
	// detailBytes is what this card has already spent of cardDetailBudget, counted on the wire (wireBytes).
	detailBytes int
	// detailed records that details is non-empty, because the field APPENDS with no separator of its own:
	// Slack stored `$ first` + `$ second` as `$ first$ second` (measured 2026-08-04). Every write after the
	// first must therefore carry its own newline.
	detailed bool
	// truncated records that cardTruncated has been written, so it is written exactly once.
	truncated bool

	// inFlight is how many of this group's calls are executing. The card resolves when it reaches zero,
	// which is a COUNT rather than a boolean because nothing in the journal promises the calls of one burst
	// are strictly sequential — they are today, and a card that assumed it would resolve on the first
	// terminal and then be reopened by a straggler.
	inFlight int
	// outcome is the worst terminal any member reached ("" while none has failed). It is what stops a group
	// with one failed call from reporting itself complete.
	outcome string
	// lastCaption is the newest call's caption, which is what the container headline mirrors while this
	// card is the one running. It is held rather than recomputed because the headline is written from the
	// card, and only the card knows which of its calls is the current one.
	lastCaption string
}

// title is what the card's headline says.
//
// A SINGLE CALL PUTS ITS CAPTION HERE rather than in the details, and that is deliberate against an
// uncertainty this package cannot settle: the API stores `details` but nothing readable says whether Slack
// RENDERS it inline under the title or behind a per-card expand. A caption in the title is visible either
// way, so the common case — one call, one line — never depends on the answer.
//
// The cost is that the title CHANGES when a second call joins: `$ ls -la` becomes `Running a command ×2`,
// and the caption it was showing moves into the list below. That flip is visible to someone watching, and it
// is the price of not hiding a lone step's caption behind a possible expand.
func (c *stepCard) title() string {
	if c.thinking {
		return thinkingCardTitle(c.thinkingLead)
	}
	if c.calls > 1 {
		return fmt.Sprintf("%s ×%d", c.phrase, c.calls)
	}
	if c.first != "" {
		return c.first
	}
	return c.phrase
}

// status maps the group onto the card vocabulary blocks.go renders.
//
// A GROUP IS NEVER `done` WHILE ANYTHING IN IT IS RUNNING and never `done` if anything in it failed. The
// second half is the one that matters: reporting a burst complete because its last call succeeded would hide
// the failure of an earlier one, and the whole point of the status pill is that it cannot be read as more
// finished than the work was.
func (c *stepCard) status() string {
	if c.inFlight > 0 {
		return "in_progress"
	}
	if c.outcome != "" {
		return c.outcome
	}
	return "done"
}

// addCaption returns what to append to details for a caption, "" when there is nothing to add. It owns the
// separator and BOTH caps, so no caller can forget either.
func (c *stepCard) addCaption(caption string) string {
	if caption == "" || c.truncated {
		return ""
	}
	if c.lines >= maxCardLines {
		return c.cut(cardTruncated)
	}
	line := caption
	if c.detailed {
		line = "\n" + line
	}
	if !c.spend(line) {
		return c.cut(cardTruncated)
	}
	c.detailed = true
	c.lines++
	return line
}

// addThinking returns what to append to details for ONE reasoning window — RAW, with no separator of its
// own, which is the opposite of what every other writer here does.
//
// THAT IS MEASURED AND IT IS THE WHOLE REASON THIS IS NOT addCaption. The windows are FRAGMENTS OF ONE
// CONTINUOUS TEXT, not a list of items: the coalescing sink (execution/model_delta_sink.go) flushes on a
// 500ms ticker, wherever the token stream happens to be. The three windows of a real step on this
// deployment (2026-08-05, session events seq 5/6/8) were
//
//	"…I need to clarify whether the range is inclusive or exclusive…"
//	"\n\nI'll go with the inclusive interpretation (17 through 23) since that's the more common approach for"
//	" such problems. The primes are 17, 19, and 23…"
//
// — the second ends mid-clause and the third opens with the space that continues it. A newline between
// them would break the sentence the model wrote; `details` appending with NO separator of its own is
// exactly what reassembles the stream, and it was verified end to end against the live API on the same
// day (three fragments sent as three chunks read back as the one continuous sentence).
//
// THE OVERFLOW KEEPS WHAT FITS instead of dropping the window. Reasoning has no second authoritative
// copy anywhere — the coordinator's own ceiling is documented that way, and it is why the note below is
// written into the card at all rather than the cut being silent.
func (c *stepCard) addThinking(text string) string {
	if text == "" || c.truncated {
		return ""
	}
	if c.spend(text) {
		c.detailed = true
		return text
	}
	kept := clipToWire(text, cardDetailBudget-c.detailBytes)
	c.detailBytes += wireBytes(kept)
	c.detailed = c.detailed || kept != ""
	return kept + c.cut(thinkingTruncated)
}

// spend books text against the card's details budget, reporting false — and booking nothing — when it
// does not fit.
func (c *stepCard) spend(text string) bool {
	cost := wireBytes(text)
	if c.detailBytes+cost > cardDetailBudget {
		return false
	}
	c.detailBytes += cost
	return true
}

// cut writes the note that says this card's details stopped, exactly once, and closes them for good.
// The reserve held back by cardDetailBudget is what guarantees the note itself fits.
func (c *stepCard) cut(note string) string {
	if c.truncated {
		return ""
	}
	c.truncated = true
	if c.detailed {
		note = "\n" + note
	}
	c.detailed = true
	return note
}

// runOutcome is what ONE run terminal means to the three surfaces that have to agree about it.
type runOutcome struct {
	// card is the status any step still open is resolved to. A run that died mid-tool did not finish that
	// tool, and a card left `in_progress` when chat.stopStream lands is stored as `error` anyway (measured
	// 2026-08-04) — so this says out loud what Slack would otherwise infer.
	card string
	// verb opens the closing headline: "Done · 33 steps · 6m 24s".
	verb string
	// notice is the line written into the message BODY, and it is empty for exactly one terminal. A run that
	// completed says what it has to say in its own words; a run that failed, was canceled, timed out or ran
	// out of budget has no words of its own, and the operator's failed run proved what that leaves behind —
	// a red card, a thirty-three step list and not one sentence telling a human the run had ended at all.
	notice string
}

// runTerminals is the ONE table of run-ending event types: the set isRunTerminal answers from, the status a
// still-open card is resolved to, the closing headline's verb and the body line each one owes a reader.
//
// It MIRRORS terminalEventTypes in apps/control-plane/api/events.go — the SSE endpoint's own closing set —
// rather than importing it, because that map is unexported there. A run terminal added to one and not the
// other is a real drift risk, catchable only by re-measuring both sides.
var runTerminals = map[string]runOutcome{
	"run.completed.v1": {card: "done", verb: "Done"},
	"run.failed.v1": {card: "failed", verb: "Failed",
		notice: "\n\n⚠️ The run failed."},
	"run.canceled.v1": {card: "canceled", verb: "Canceled",
		notice: "\n\n⏹️ The run was canceled."},
	"run.timed_out.v1": {card: "failed", verb: "Timed out",
		notice: "\n\n⌛ The run timed out."},
	"run.budget_exceeded.v1": {card: "failed", verb: "Stopped",
		notice: "\n\n⚠️ The run stopped — its budget was exhausted."},
}

// isRunTerminal reports whether an event ends the run. ONE map serves this and everything the terminal
// decides, because two tables listing the same five types is a drift the compiler cannot see.
func isRunTerminal(eventType string) bool {
	_, ok := runTerminals[eventType]
	return ok
}

// headline composes the closing container title: what happened, how much of it there was, and how long a
// reader waited for it.
//
// THE STEP COUNT IS THE CALL COUNT, NOT THE CARD COUNT. Cards group, so a run of thirty-three calls draws
// about ten of them, and a headline reading "10 steps" would be counting this file's own rendering rather
// than the run's work.
//
// A run with no steps prints no count. That is most real runs today — E08 exposes no tools to a real
// provider, so a single-step answer has nothing to tally — and "Done · 0 steps · 4s" would be an odd way to
// say "it just answered".
func (o runOutcome) headline(steps int, elapsed time.Duration) string {
	switch steps {
	case 0:
		return fmt.Sprintf("%s · %s", o.verb, formatElapsed(elapsed))
	case 1:
		return fmt.Sprintf("%s · 1 step · %s", o.verb, formatElapsed(elapsed))
	default:
		return fmt.Sprintf("%s · %d steps · %s", o.verb, steps, formatElapsed(elapsed))
	}
}

// formatElapsed renders how long the run took, at the coarsest granularity that is still honest.
//
// IT NEVER RETURNS EMPTY, including for a duration of zero. A formatter that dropped sub-second runs would
// take the duration out of every fast test's expectation and leave the one thing this exists to show —
// that a person waited six and a half minutes — asserted nowhere.
func formatElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// live is the headline while the run is still going: the caption of the step that is running, or the tool's
// phrase when that step has no caption to show.
//
// THIS LINE IS THE WHOLE ANSWER TO "sürekli dönüyor". The container's title is the only thing a collapsed
// reader sees and the only thing visible for the entire length of a run; left to Slack it reads `Thinking`
// from the first second to the last. Mirroring the running step here means it changes on every step, and
// what it changes to is the most specific thing known at that moment.
func (c *stepCard) live() string {
	if c.lastCaption != "" {
		return c.lastCaption
	}
	return c.phrase
}

// openStreamCards is the card state ONE Run accumulates. It is a type rather than four fields on openStream
// so that the grouping rule lives next to the cards it groups.
type openStreamCards struct {
	// current is the card the next call joins if it is for the same tool, and nil before the first one.
	current *stepCard
	// byCall maps a tool_call_id to the card that call belongs to, because the events that RESOLVE a call —
	// and the progress notifications that decorate it — name the call, never the group.
	byCall map[string]*stepCard
	// thinkingByStep maps a model step's `model_request_id` to the card holding that step's reasoning. It
	// is a SECOND index rather than an entry in byCall because the two vocabularies name different
	// things: a tool_call_id names a call and a model_request_id names the step that decided on it, and
	// one map holding both would make a collision between two id spaces this package does not mint into
	// a card silently rendering one thing as the other.
	thinkingByStep map[string]*stepCard
	// all is every card drawn, in order, so a run that ends mid-step can resolve the ones still open.
	all []*stepCard
	// steps counts tool calls, which is what the closing headline reports.
	steps int
}

func newCards() *openStreamCards {
	return &openStreamCards{byCall: map[string]*stepCard{}, thinkingByStep: map[string]*stepCard{}}
}

// think records one reasoning window and answers the card to draw, the detail to append, and the
// headline the container should now show. A nil card means there is nothing to draw.
//
// A WINDOW WITH NO model_request_id DRAWS NOTHING, for the reason begin() gives about a call with no id:
// an empty id is not a card Slack can advance, and two model steps sharing one would overwrite each
// other's reasoning, which is worse than the reasoning not being drawn. A window with no text draws
// nothing either — the coordinator refuses to journal an empty one, so this is a malformed event rather
// than a step that thought nothing.
func (s *openStreamCards) think(data map[string]any) (*stepCard, string, string) {
	id := dataString(data, "model_request_id")
	text := dataString(data, "thinking")
	if id == "" || text == "" {
		return nil, "", ""
	}
	card := s.thinkingByStep[id]
	if card == nil {
		// inFlight opens at one: the step IS running, and the card is resolved by that step's own
		// terminal (endThinking) or, failing that, by the run's (runEnded). It is deliberately not drawn
		// `done` on arrival — a window is a record of reasoning that has happened, but the step it
		// belongs to has not, and a card that reported itself complete while the model was still
		// working would be the "Thinking… forever" defect in a new place.
		card = &stepCard{id: id, thinking: true, inFlight: 1}
		s.thinkingByStep[id] = card
		s.all = append(s.all, card)
	}
	card.growLead(text)
	return card, card.addThinking(text), thinkingCardTitle(card.thinkingLead)
}

// growLead extends the card's lead line — its title, and the container headline while this step runs.
//
// IT IS GROWN RATHER THAN TAKEN WHOLE FROM THE FIRST WINDOW because a window is not a sentence: the sink
// flushes on a timer, so the first one is whatever had streamed in 500ms. It could be a full paragraph
// (232 bytes, measured 2026-08-05) or three words. Growing until the reasoning's first line actually
// ENDS is what makes the title say something either way.
//
// AND IT STOPS, which is the half that costs money. The headline is written with a chat.appendStream
// chunk against a shared Tier 4 budget, and setHeadline suppresses a repeat — so a lead that stopped
// changing is a lead that stops spending. Closing it at the first newline OR at a headline's worth of
// runes bounds that to about two writes per model step, whatever the provider does with its windows.
func (c *stepCard) growLead(text string) {
	if c.leadClosed {
		return
	}
	line, cut := text, false
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line, cut = line[:i], true
	}
	if c.thinkingLead == "" {
		line = strings.TrimLeft(line, " \t")
	}
	c.thinkingLead += line
	if cut || len([]rune(c.thinkingLead)) >= maxHeadline {
		c.leadClosed = true
	}
}

// endThinking resolves the reasoning card of a model step that has just ended, or nil when this step
// produced no reasoning — which is most steps in most runs and is not a failure.
func (s *openStreamCards) endThinking(data map[string]any, status string) *stepCard {
	card := s.thinkingByStep[dataString(data, "model_request_id")]
	if card == nil {
		return nil
	}
	card.inFlight = 0
	if status != "done" && card.outcome == "" {
		card.outcome = status
	}
	return card
}

// begin records a call starting and answers the card to draw plus the detail line to append.
//
// A CALL WITH NO tool_call_id DRAWS NOTHING, unchanged from before this file existed: an empty id is not a
// card Slack can update, and two steps sharing one would overwrite each other, which is worse than the step
// not being drawn. Nothing a reader needs is lost — the answer text travels by an entirely separate path.
func (s *openStreamCards) begin(data map[string]any) (*stepCard, string) {
	id := dataString(data, "tool_call_id")
	if id == "" {
		return nil, ""
	}
	name := toolName(data)
	caption := stepCaption(data)
	s.steps++

	if s.current == nil || s.current.tool != name {
		card := &stepCard{id: id, tool: name, phrase: toolTitle(data)}
		s.current = card
		s.all = append(s.all, card)
	}
	card := s.current
	s.byCall[id] = card
	card.calls++
	card.inFlight++
	card.lastCaption = caption

	switch {
	case card.calls == 1:
		// The caption rides the TITLE while this is the only call — see stepCard.title.
		card.first = caption
		return card, ""
	case card.calls == 2:
		// The group just became a group: the first caption leaves the title and joins the second one in the
		// list, in ONE write, because a card's details take one append per chunk and two chunks here would
		// draw the same card twice.
		return card, card.addCaption(card.first) + card.addCaption(caption)
	default:
		return card, card.addCaption(caption)
	}
}

// resolve records a call ending. It answers nil for a call no card is holding, which is every call whose
// executing frame this relay never saw — a stream joined late, or a tool whose class needs no pre-write.
func (s *openStreamCards) resolve(data map[string]any, status string) *stepCard {
	card := s.byCall[dataString(data, "tool_call_id")]
	if card == nil {
		return nil
	}
	if card.inFlight > 0 {
		card.inFlight--
	}
	if status != "done" && card.outcome == "" {
		card.outcome = status
	}
	return card
}

// progress finds the card a progress notification decorates. Those notifications carry a tool_call_id and
// nothing else — no tool_name (packages/coordinator/mcp_progress.go AppendToolProgress) — which is why the
// card has to be found by id rather than rebuilt from the event.
func (s *openStreamCards) progress(data map[string]any) *stepCard {
	return s.byCall[dataString(data, "tool_call_id")]
}

// task renders a card as the chunk the wire takes. The detail is passed in rather than held on the card
// because `details` APPENDS: what goes on the wire is the NEW text only, and sending what is already there
// would double it.
func (c *stepCard) task(detail string) slack.Task {
	return slack.Task{ID: c.id, Title: c.title(), Status: c.status(), Detail: detail}
}

// maxHeadline bounds the container's title, in RUNES.
//
// MEASURED FROM A REAL RUN rather than picked: the live leg on 2026-08-04 mirrored a step whose caption was
//
//	$ bash -lc find . -name '*.swift' -not -path '*/.build/*' | wc -l && echo --- && find . -name '*.swift'
//	-not -path '*/.build/*' -exec wc -l {} + | sort -rn | head -10
//
// — 197 characters into a field that is ONE line. `arguments_summary` is capped at 256 bytes upstream, which
// is the right bound for a card's row and far too much for a headline. The card still carries the whole
// caption; this cut applies only to the mirrored copy.
const maxHeadline = 80

// trimHeadline keeps a container title to one readable line.
//
// TWO CUTS, AND THE SECOND IS NOT COSMETIC. A caption is one line by construction (toolbroker.ArgumentSummary
// renders exactly one), but the field is the model's words downstream of a contract this package does not
// own, and a headline that grew a newline would take the container's single visible line and turn it into
// several. The length cut is what keeps that line readable when the caption is a hundred-character pipeline.
//
// Both are marked with an ellipsis, because a headline a reader cannot tell is truncated is a headline that
// says the command ended where it did not.
func trimHeadline(text string) string {
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = strings.TrimSpace(text[:i]) + " …"
	}
	if runes := []rune(text); len(runes) > maxHeadline {
		return strings.TrimRight(string(runes[:maxHeadline-1]), " ") + "…"
	}
	return text
}
