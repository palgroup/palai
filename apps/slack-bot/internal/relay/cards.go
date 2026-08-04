package relay

import (
	"fmt"
	"strings"
	"time"

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

	calls int
	// first is the opening call's caption, kept because it lives in the TITLE while the group has one member
	// and has to move down into the list when a second one arrives.
	first string
	// lines counts captions already written into details, against maxCardLines.
	lines int
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
// separator and the cap, so no caller can forget either.
func (c *stepCard) addCaption(caption string) string {
	if caption == "" || c.truncated {
		return ""
	}
	if c.lines >= maxCardLines {
		c.truncated = true
		return c.separated(cardTruncated)
	}
	c.lines++
	return c.separated(caption)
}

func (c *stepCard) separated(line string) string {
	if c.detailed {
		return "\n" + line
	}
	c.detailed = true
	return line
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
	// all is every card drawn, in order, so a run that ends mid-step can resolve the ones still open.
	all []*stepCard
	// steps counts tool calls, which is what the closing headline reports.
	steps int
}

func newCards() *openStreamCards {
	return &openStreamCards{byCall: map[string]*stepCard{}}
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
