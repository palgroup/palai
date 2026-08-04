package relay

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// shellStep is one tool_call.executing.v1 for the shell tool, with the caption the journal really carries.
// It is a helper rather than a literal per fixture because the FIELD NAME is the whole subject of this file:
// a test that typed `arguments_summary` correctly in one place and `argument_summary` in another would go
// green on the fallback path while claiming to measure the caption path.
func shellStep(id, summary string) palai.Event {
	data := map[string]any{"tool_call_id": id, "tool_name": "palai.workspace.shell", "replay_class": "irreversible"}
	if summary != "" {
		data["arguments_summary"] = summary
	}
	return palai.Event{Type: "tool_call.executing.v1", Data: data}
}

func doneStep(id, tool string) palai.Event {
	return palai.Event{Type: "tool_call.completed.v1",
		Data: map[string]any{"tool_call_id": id, "tool_name": tool}}
}

func runCards(t *testing.T, events []palai.Event) *fakeSlack {
	t.Helper()
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals, Delivery: &recordedDelivery{}},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return fake
}

// TestAStepSaysWhatItRan is the complaint, closed. The operator's thread showed thirty-three rows reading
// "Running a command" and "Reading files" with no detail on any of them, and the reason was that the journal
// had nothing else to give: an executing frame was exactly {run_id, tool_name, tool_call_id, replay_class}.
// It now carries `arguments_summary` (commit 9172fea9), and a card that has one says it.
//
// THE ASSERTION IS ON THE TITLE, not on "the summary appears somewhere", because where it appears is the
// whole point. A caption in the card's `details` may sit behind a per-card expand; the title is the line
// that is visible in the list either way, which is why a lone step puts its caption there.
func TestAStepSaysWhatItRan(t *testing.T) {
	fake := runCards(t, []palai.Event{
		shellStep("tc_1", `grep -rln "Date of birth" repo`),
		doneStep("tc_1", "palai.workspace.shell"),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	const want = `$ grep -rln "Date of birth" repo`
	if len(fake.tasks) != 2 {
		t.Fatalf("drew %+v, want the executing/completed pair for one card", fake.tasks)
	}
	for i, task := range fake.tasks {
		if task.Title != want {
			t.Fatalf("update %d titled %q, want %q — the caption is the answer to \"ne okudu ne etti\", and a "+
				"card still titled `Running a command` has not answered it", i, task.Title, want)
		}
	}
}

// TestACaptionIsPrefixedByWhatTheSummaryCannotSayAboutItself walks captionPrefix's OWN table rather than a
// list retyped here, so an entry added without a rendering rule cannot ship untested.
//
// THE SUMMARY IS OPAQUE and this is the one place that knowledge lives. A shell summary IS a command line and
// takes a prompt glyph; the file tool's and the editor's already lead with their own verb and take nothing; a
// commit subject and a pull request title read as bare prose and are labelled.
func TestACaptionIsPrefixedByWhatTheSummaryCannotSayAboutItself(t *testing.T) {
	for tool, prefix := range captionPrefix {
		t.Run(tool, func(t *testing.T) {
			const summary = "SUMMARY"
			got := stepCaption(map[string]any{"tool_name": tool, "arguments_summary": summary})
			if got != prefix+summary {
				t.Fatalf("caption = %q, want %q", got, prefix+summary)
			}
			if !strings.Contains(got, summary) {
				t.Fatalf("caption = %q and has lost the summary itself", got)
			}
		})
	}
}

// TestASummarisedToolNobodyPrefixedStillReadsAsASentence is the DRIFT case, and it is the reason
// captionPrefix mirroring an upstream map is safe rather than a coupling. toolbroker.ArgumentSummary owns
// which tools get a summary; if a summariser is added there and no prefix is added here, the caption falls
// back to the tool's human phrase followed by the summary — longer than a tailored line, never wrong and
// never missing.
func TestASummarisedToolNobodyPrefixedStillReadsAsASentence(t *testing.T) {
	// palai.web.search has a phrase in toolTitles and deliberately no captionPrefix entry.
	if _, prefixed := captionPrefix["palai.web.search"]; prefixed {
		t.Fatal("this fixture needs a tool with a phrase and NO prefix; palai.web.search now has one")
	}
	got := stepCaption(map[string]any{"tool_name": "palai.web.search", "arguments_summary": "swiftui date picker"})
	if got != "Searching the web: swiftui date picker" {
		t.Fatalf("caption = %q, want the phrase and the summary joined", got)
	}
	// And a tool with NEITHER a phrase nor a prefix falls back to its registered name, which is the most
	// informative thing available for an MCP server's tool.
	got = stepCaption(map[string]any{"tool_name": "acme.jira.create", "arguments_summary": "PROJ-12"})
	if got != "acme.jira.create: PROJ-12" {
		t.Fatalf("caption = %q, want the raw name and the summary joined", got)
	}
}

// TestNoCaptionIsInventedWhenTheFrameHasNone is the MCP and remote-HTTP case, which is not an edge: those
// tools are deliberately withheld a summary upstream because a third party owns their argument schema and it
// may grow an `api_key` property tomorrow. A card for one of them must read exactly as every card read
// before this change — the tool's human phrase, and nothing under it.
func TestNoCaptionIsInventedWhenTheFrameHasNone(t *testing.T) {
	if got := stepCaption(map[string]any{"tool_name": "palai.workspace.shell"}); got != "" {
		t.Fatalf("caption = %q for a frame with no arguments_summary, want none — a placeholder repeating the "+
			"tool's phrase is exactly the row the operator could not read", got)
	}
	fake := runCards(t, []palai.Event{
		shellStep("tc_1", ""),
		doneStep("tc_1", "palai.workspace.shell"),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	for i, task := range fake.tasks {
		if task.Title != "Running a command" || task.Detail != "" {
			t.Fatalf("update %d = %+v, want the human phrase and no detail", i, task)
		}
	}
}

// TestConsecutiveCallsToOneToolShareOneCard is the grouping, and the sequence it asserts is the whole
// mechanism: the first call's caption lives in the TITLE, and the moment a second call joins, that caption
// moves down into the list and the title becomes the group's header.
//
// THE MOVE HAPPENS IN ONE UPDATE, which is why the second update's detail carries BOTH captions. `details`
// APPENDS (Slack stored "$ first" + "$ second" as "$ first$ second", measured 2026-08-04), so two updates
// here would draw the same card twice and cost a Tier 4 call for nothing.
func TestConsecutiveCallsToOneToolShareOneCard(t *testing.T) {
	fake := runCards(t, []palai.Event{
		shellStep("tc_1", "ls -la Sources"),
		doneStep("tc_1", "palai.workspace.shell"),
		{Type: "model_step.created.v1", Data: map[string]any{}},   // a model step does NOT break the group
		{Type: "model_step.completed.v1", Data: map[string]any{}}, //
		shellStep("tc_2", "swift build"),
		doneStep("tc_2", "palai.workspace.shell"),
		shellStep("tc_3", "git status"),
		doneStep("tc_3", "palai.workspace.shell"),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	for i, task := range fake.tasks {
		if task.ID != "tc_1" {
			t.Fatalf("update %d carried id %q, want tc_1 for every member of one group — a differing id draws "+
				"a SECOND card and the run is back to a row per call", i, task.ID)
		}
	}
	var titles, details []string
	for _, task := range fake.tasks {
		titles = append(titles, task.Title)
		details = append(details, task.Detail)
	}
	wantTitles := []string{
		"$ ls -la Sources",     // one call: its caption IS the title
		"$ ls -la Sources",     // it finished; still one call
		"Running a command ×2", // the second call joined
		"Running a command ×2", //
		"Running a command ×3", //
		"Running a command ×3", //
	}
	if !slices.Equal(titles, wantTitles) {
		t.Fatalf("titles were\n  %q\nwant\n  %q", titles, wantTitles)
	}
	wantDetails := []string{
		"",
		"",
		"$ ls -la Sources\n$ swift build", // the first caption moves down beside the second, in ONE write
		"",
		"\n$ git status", // and every later caption carries its own separator
		"",
	}
	if !slices.Equal(details, wantDetails) {
		t.Fatalf("details were\n  %q\nwant\n  %q", details, wantDetails)
	}
	// EVERY CAPTION SURVIVES SOMEWHERE. Grouping must shorten the list, never lose a line: this is the
	// property that separates it from simply drawing fewer cards.
	whole := strings.Join(details, "")
	for _, caption := range []string{"ls -la Sources", "swift build", "git status"} {
		if !strings.Contains(whole, caption) {
			t.Fatalf("%q reached no card at all; grouping may shorten the list but must not drop a step", caption)
		}
	}
}

// TestADifferentToolStartsANewCard is grouping's other half. Collapsing every call of one tool across a
// whole run would give a shorter list still — and would reorder the story, so a reader could no longer see
// that the bot read a file BETWEEN two commands. Only CONSECUTIVE calls share a card.
func TestADifferentToolStartsANewCard(t *testing.T) {
	fileStep := palai.Event{Type: "tool_call.executing.v1", Data: map[string]any{
		"tool_call_id": "tc_2", "tool_name": "palai.workspace.file", "arguments_summary": "read repo/RootView.swift"}}
	fake := runCards(t, []palai.Event{
		shellStep("tc_1", "ls -la"),
		doneStep("tc_1", "palai.workspace.shell"),
		fileStep,
		doneStep("tc_2", "palai.workspace.file"),
		shellStep("tc_3", "swift build"),
		doneStep("tc_3", "palai.workspace.shell"),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	var ids []string
	for _, task := range fake.tasks {
		if len(ids) == 0 || ids[len(ids)-1] != task.ID {
			ids = append(ids, task.ID)
		}
	}
	if want := []string{"tc_1", "tc_2", "tc_3"}; !slices.Equal(ids, want) {
		t.Fatalf("cards were %v, want %v — the shell call AFTER the file read must not rejoin the shell card "+
			"before it, or the order of the run is lost", ids, want)
	}
	for _, task := range fake.tasks {
		if task.ID == "tc_3" && task.Title != "$ swift build" {
			t.Fatalf("the reopened shell card is titled %q, want its own caption — it is a NEW group of one",
				task.Title)
		}
	}
}

// TestAGroupNeverGrowsWithoutBound: a card's list is capped, and the cut SAYS SO. A run that made a hundred
// calls to one tool must not write a hundred lines into one chunk, and a reader who sees twenty lines under
// a title reading ×100 must be able to tell why.
//
// THE CAP IS OURS BECAUSE SLACK'S IS NOT KNOWABLE: the reference documents a 256-character limit on a
// task_update chunk and the live API does not enforce it (measured 2026-08-04 — 300 characters in one write
// and 500 accumulated were both stored intact). A limit that is documented but unapplied is the worst kind
// to lean on.
func TestAGroupNeverGrowsWithoutBound(t *testing.T) {
	var events []palai.Event
	const calls = maxCardLines + 6
	for i := range calls {
		id := fmt.Sprintf("tc_%d", i)
		events = append(events, shellStep(id, fmt.Sprintf("echo %d", i)), doneStep(id, "palai.workspace.shell"))
	}
	events = append(events, palai.Event{Type: "run.completed.v1", Data: map[string]any{}})
	fake := runCards(t, events)

	whole := strings.Join(collectDetails(fake.tasks), "")
	if lines := strings.Count(whole, "\n") + 1; lines > maxCardLines+1 {
		t.Fatalf("the card listed %d lines for %d calls, want at most %d captions plus the truncation notice",
			lines, calls, maxCardLines)
	}
	if strings.Count(whole, cardTruncated) != 1 {
		t.Fatalf("the truncation notice appears %d time(s); it must be written exactly once — never zero (a "+
			"silent cut) and never per call", strings.Count(whole, cardTruncated))
	}
	// The TITLE still counts every call, which is what makes the cut legible rather than a lie: twenty lines
	// under a header saying ×26 is a reader being told that six are missing.
	last := fake.tasks[len(fake.tasks)-1]
	if want := fmt.Sprintf("Running a command ×%d", calls); last.Title != want {
		t.Fatalf("the final title is %q, want %q — the count must keep counting past the listed lines, or the "+
			"truncation has no denominator", last.Title, want)
	}
}

func collectDetails(tasks []slack.Task) []string {
	var out []string
	for _, task := range tasks {
		if task.Detail != "" {
			out = append(out, task.Detail)
		}
	}
	return out
}

// TestAGroupWithOneFailedCallDoesNotReportItselfComplete: a burst whose last call succeeded must not hide
// that an earlier one did not. The status pill is the one thing on a card that cannot be allowed to read as
// more finished than the work was.
func TestAGroupWithOneFailedCallDoesNotReportItselfComplete(t *testing.T) {
	fake := runCards(t, []palai.Event{
		shellStep("tc_1", "swift build"),
		{Type: "tool_call.uncertain.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.shell"}},
		shellStep("tc_2", "git status"),
		doneStep("tc_2", "palai.workspace.shell"),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	last := fake.tasks[len(fake.tasks)-1]
	if last.Status != "failed" {
		t.Fatalf("the group ended %q though one of its calls went uncertain; want failed", last.Status)
	}
	if slack.TaskStatus(last.Status) != "error" {
		t.Fatalf("the group's status renders as %q on the wire, want error", slack.TaskStatus(last.Status))
	}
}

// TestAStepLeftOpenIsResolvedByTheRunTerminal is SLK-P2 on a card. A run killed mid-tool leaves its step
// `in_progress`, and Slack stores such a card as `error` when chat.stopStream lands (measured 2026-08-04) —
// so an unresolved card does not merely look unfinished, it reports a failure. Saying the run's own outcome
// explicitly is what makes the card report the run rather than an inference.
func TestAStepLeftOpenIsResolvedByTheRunTerminal(t *testing.T) {
	for term, outcome := range runTerminals {
		t.Run(term, func(t *testing.T) {
			fake := runCards(t, []palai.Event{
				shellStep("tc_1", "swift build"), // no terminal for this call: the run dies under it
				{Type: term, Data: map[string]any{}},
			})
			last := fake.tasks[len(fake.tasks)-1]
			if last.Status != outcome.card {
				t.Fatalf("%s left the step at %q, want %q", term, last.Status, outcome.card)
			}
			if last.Status == "in_progress" {
				t.Fatalf("%s left a step spinning in a finished message", term)
			}
			if slack.TaskStatus(last.Status) == "pending" {
				t.Fatalf("%s maps the step to %q, which blocks.go renders as pending — a finished run must not "+
					"leave a step looking not-yet-started", term, last.Status)
			}
			// The title must ride the closing update too: a task_update that omits `title` CLEARS it
			// (measured 2026-08-04 — a chunk of only {id,status} stored the card with title "").
			if last.Title == "" {
				t.Fatalf("%s resolved the card with no title, which BLANKS it on the wire", term)
			}
		})
	}
}

// TestTheHeadlineNarratesTheRun is the answer to "sürekli dönüyor". Left alone, Slack titles the container
// `Thinking` and leaves it there for the whole run — six minutes and twenty-four seconds of one unchanging
// word in the operator's own thread. The headline is the only line visible while the container is collapsed,
// so it is where the run has to say what it is doing.
func TestTheHeadlineNarratesTheRun(t *testing.T) {
	fake := runCards(t, []palai.Event{
		{Type: "model_step.created.v1", Data: map[string]any{}},
		{Type: "model_step.created.v1", Data: map[string]any{}}, // deduped: the headline already says this
		{Type: "model_step.completed.v1", Data: map[string]any{}},
		shellStep("tc_1", "swift build"),
		doneStep("tc_1", "palai.workspace.shell"),
		{Type: "model_step.created.v1", Data: map[string]any{}},
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "Built."}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	if len(fake.plans) != 5 {
		t.Fatalf("wrote %d headlines (%q), want 5 — the opening, Thinking, the step, Thinking again, and the "+
			"closing summary", len(fake.plans), fake.plans)
	}
	want := []string{openingHeadline, thinkingHeadline, "$ swift build", thinkingHeadline}
	if !slices.Equal(fake.plans[:4], want) {
		t.Fatalf("headlines were %q, want %q followed by a summary", fake.plans, want)
	}
	// THE CLOSING LINE reports the work and the wait. It is asserted by prefix because the elapsed time is
	// real wall clock; formatElapsed is pinned exhaustively as a pure function by TestFormatElapsed.
	closing := fake.plans[4]
	if !strings.HasPrefix(closing, "Done · 1 step · ") {
		t.Fatalf("the closing headline is %q, want it to open with the outcome and the step count", closing)
	}
	if strings.TrimPrefix(closing, "Done · 1 step · ") == "" {
		t.Fatalf("the closing headline is %q and carries no elapsed time — the wait is the half of this a "+
			"reader cannot reconstruct", closing)
	}
	// AND NOTHING ABOUT ANY OF IT REACHES THE BODY. A headline is the container's, not the answer's.
	body := fake.startedText + strings.Join(fake.appended, "") + fake.stoppedText
	if body != "Built." {
		t.Fatalf("body = %q, want only the model's own words", body)
	}
}

// TestEveryRunTerminalWritesItsOwnClosingHeadline walks runTerminals' OWN table for the same reason the
// tool-call test walks the state machine's: a terminal added to the map without being exercised here would
// ship untested, and a list retyped here could drift from what Run consults.
func TestEveryRunTerminalWritesItsOwnClosingHeadline(t *testing.T) {
	seen := map[string]bool{}
	for term, outcome := range runTerminals {
		t.Run(term, func(t *testing.T) {
			fake := runCards(t, []palai.Event{{Type: term, Data: map[string]any{}}})
			if len(fake.plans) != 2 {
				t.Fatalf("%s wrote %q, want the opening headline and one closing one", term, fake.plans)
			}
			closing := fake.plans[1]
			if !strings.HasPrefix(closing, outcome.verb+" · ") {
				t.Fatalf("%s closed with %q, want it to open with %q", term, closing, outcome.verb)
			}
			// A run with NO steps prints no count: that is most real runs today (E08 exposes no tools to a
			// real provider), and "Done · 0 steps" is an odd way to say "it just answered".
			if strings.Contains(closing, "0 step") {
				t.Fatalf("%s closed with %q — a run that ran nothing must not tally it", term, closing)
			}
			if seen[closing] {
				t.Fatalf("%s produced a headline another terminal already produced (%q); the four ways a run "+
					"can end badly must not read identically", term, closing)
			}
			seen[closing] = true
		})
	}
}

// TestARunThatEndsBadlySaysSoInWords is the silence the operator actually hit. Their run made thirty-three
// tool calls over six and a half minutes, failed, and left a message whose BODY WAS EMPTY — a red card and
// not one sentence telling a human what had happened. A terminal that is not `completed` has no words of its
// own by definition, so this is the only place they can come from.
func TestARunThatEndsBadlySaysSoInWords(t *testing.T) {
	for term, outcome := range runTerminals {
		t.Run(term, func(t *testing.T) {
			fake := runCards(t, []palai.Event{
				{Type: "model_step.delta.v1", Data: map[string]any{"text": "Partial thought."}},
				{Type: term, Data: map[string]any{}},
			})
			body := fake.startedText + strings.Join(fake.appended, "") + fake.stoppedText
			if term == "run.completed.v1" {
				if body != "Partial thought." {
					t.Fatalf("a completed run added %q to the body; the model's answer speaks for itself", body)
				}
				return
			}
			if !strings.Contains(body, strings.TrimSpace(outcome.notice)) {
				t.Fatalf("%s left the body %q with no word about the ending — the operator's failed run showed "+
					"a red card and nothing else", term, body)
			}
			// The model's own words are never displaced by the notice.
			if !strings.Contains(body, "Partial thought.") {
				t.Fatalf("%s lost what the model had already said: body = %q", term, body)
			}
			// And the notice never fuses to what precedes it — chat.appendStream APPENDS, which is how
			// `doneProjede` happened.
			idx := strings.Index(body, strings.TrimSpace(outcome.notice))
			if idx > 0 && body[idx-1] != '\n' {
				t.Fatalf("%s glued its notice to the text before it: %q", term, body[max(0, idx-20):idx+20])
			}
		})
	}
}

// TestFormatElapsed pins the formatter exhaustively as a pure function, which is what lets the headline
// tests above assert by prefix without leaving the duration measured nowhere.
//
// IT NEVER RETURNS EMPTY, including at zero: a formatter that dropped sub-second runs would be absent from
// every fast test's expectation and present only in production, which is the shape of a thing nobody ever
// checks.
func TestFormatElapsed(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{400 * time.Millisecond, "0s"},
		{1500 * time.Millisecond, "2s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m 00s"},
		{384 * time.Second, "6m 24s"}, // the operator's own run
		{59*time.Minute + 59*time.Second, "59m 59s"},
		{time.Hour, "1h 00m"},
		{2*time.Hour + 5*time.Minute, "2h 05m"},
	} {
		if got := formatElapsed(tc.in); got != tc.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAHeadlineIsAlwaysOneLine: the container's title is its single visible line, so a newline in it would
// take that line and turn it into several. A caption is one line by construction upstream, but this is
// downstream of a contract this package does not own.
func TestAHeadlineIsAlwaysOneLine(t *testing.T) {
	if got := trimHeadline("first\nsecond"); got != "first …" {
		t.Fatalf("trimHeadline = %q, want the first line and a visible mark that it was cut", got)
	}
	if got := trimHeadline("$ swift build"); got != "$ swift build" {
		t.Fatalf("trimHeadline altered a one-line headline: %q", got)
	}
	// AND IT IS BOUNDED, from the live leg's own overflow: a step whose caption was a 197-character pipeline
	// was mirrored whole into a field that is ONE line. The card still carries the full caption; only the
	// mirrored copy is cut, and the cut is visible.
	long := "$ " + strings.Repeat("x", 300)
	got := trimHeadline(long)
	if len([]rune(got)) != maxHeadline {
		t.Fatalf("trimHeadline kept %d runes of a %d-rune caption, want %d", len([]rune(got)), len([]rune(long)), maxHeadline)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a cut headline (%q) does not say it was cut, so it reads as a command that ended there", got)
	}
	fake := runCards(t, []palai.Event{
		shellStep("tc_1", "echo one\necho two"),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	for _, plan := range fake.plans {
		if strings.ContainsAny(plan, "\r\n") {
			t.Fatalf("a headline carried a newline: %q", plan)
		}
	}
}

// TestEveryCardUpdateCarriesItsTitle is the measured trap, swept over every fixture in this file's style: a
// task_update WITHOUT `title` does not preserve the card's title, it CLEARS it. Driven against the live API
// on 2026-08-04, a card opened as `$ swift build` and then updated with only {id,status} came back stored
// with title "". So a title is not optional on any update, ever.
func TestEveryCardUpdateCarriesItsTitle(t *testing.T) {
	fake := runCards(t, []palai.Event{
		shellStep("tc_1", "ls"),
		{Type: "tool_call.progress.v1", Data: map[string]any{"tool_call_id": "tc_1", "message": "still going"}},
		doneStep("tc_1", "palai.workspace.shell"),
		shellStep("tc_2", "swift build"),
		{Type: "run.failed.v1", Data: map[string]any{}},
	})
	if len(fake.tasks) == 0 {
		t.Fatal("no cards were drawn; this sweep would be vacuous")
	}
	for i, task := range fake.tasks {
		if task.Title == "" {
			t.Fatalf("update %d (%+v) carried no title, which BLANKS the card on the wire", i, task)
		}
		if task.ID == "" {
			t.Fatalf("update %d (%+v) carried no id, so Slack would draw a new card instead of advancing one", i, task)
		}
	}
}
