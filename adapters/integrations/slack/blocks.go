package slack

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// The BLOCK KIT RENDERER (E20 T4, spec §36, plan §2). A run's answer becomes the structure Slack draws, and
// the whole file exists to make one sentence true:
//
//	THE MODEL NEVER EMITS AN ACTIONABLE ELEMENT.
//
// That is not a style rule. If a model could emit one it would draw a button that looks exactly like the
// approval buttons in this same workspace, a human would click it, and the click would have passed through
// NONE of the chain that makes an approval an approval (ApproverAuthorized -> AcceptCommand ->
// ApplyApprovalDecision). So the model produces TYPED output, this file turns it into blocks, and the sole
// mint of anything a human can act on stays interactions.go — protected by a test that sweeps this renderer's
// output AND the package's source, not by a comment (see blocks_test.go).
//
// THE UNION IS CLOSED AND THE RENDER IS TOTAL. Five variants, each with a user today: text, table (test
// results), tasks (the journal's task.* events), file_ref (an artifact, as a link), mention (E21 T3 — the
// agent asking the person who asked it). Anything else — a tag we do not know, a tag Slack knows and we did
// not choose, a malformed variant — renders as INERT TEXT. Never passthrough: the bytes are shown as
// characters, so the reader still sees what the model wrote and nobody can click it.
//
// THE MENTION EXTENDS THAT RULE RATHER THAN EXCEPTING IT, and the distinction is one sentence: the defence is
// not "the model may not address a human", it is "text the MODEL WROTE may not address a human". A `<@U…>` in
// the model's prose is still defused; the token this renderer emits is built from an id the model never
// supplies and cannot name (MentionRequester, Result.mint). Same shape as the button rule above — typed
// intent in, our mint out.
//
// WHAT IS DELIBERATELY NOT HERE, so a later reader does not read absence as oversight:
//
//   - A VIDEO BLOCK IS STRUCTURALLY IMPOSSIBLE for anything we hold (S13). video_url must be public,
//     iframe-embeddable and on one of the app's unfurl domains — a Slack-hosted file is none of those — and
//     the scope it needs (links.embed:write) is deliberately not requested.
//   - FILE UPLOAD IS NOT BUILT (S14). The real path is files.getUploadURLExternal ->
//     files.completeUploadExternal, which is a separate message rather than a block, and files:write is a new
//     standing write access. file_ref renders a link today; the path stays written down and the door shut.
//   - feedback_buttons / icon_button / context_actions are out of scope BECAUSE they are actionable: each one
//     deserves the authorization path approve/deny earned.
//   - No data-visualization, carousel, card or alert blocks. No case asks for them.
//
// CEILING: these blocks are validated SYNTACTICALLY, against the published references cited on each builder.
// How they LOOK in a real workspace is §6 leg 1 and is claimed nowhere.

// The closed union's tags. A model's output is matched against exactly these; every other tag is inert text.
const (
	ResultText    = "text"
	ResultTable   = "table"
	ResultTasks   = "tasks"
	ResultFileRef = "file_ref"
	ResultMention = "mention"
)

// MentionRequester is the ONLY value `who` takes (E21 T3, plan §2). It is a closed enum rather than a user
// id because the difference is the whole defence: an id would be a string the MODEL chose, and this renderer
// takes no identity from the model at all — it holds exactly one, the requester frozen on the delivery row,
// and `who` selects between that and nothing.
//
// ADDING A SECOND IDENTITY (say, "the approver") costs three things and none of them is a constant: a column
// that carries that id durably to the reply pump, a value here, and a test proving WE and not the model
// decide who it names.
const MentionRequester = "requester"

// The vendor's limits, every one of them a TRUNCATION POINT rather than a rejection — an answer that is one
// row too long must still reach the human, minus the rows and plus a marker saying so.
//
// CONTRACT: https://docs.slack.dev/reference/methods/chat.stopStream/ (checked 2026-07-27) — the blocks array
// is capped at 50. https://docs.slack.dev/reference/block-kit/blocks/table-block/ (checked 2026-07-27) — "100
// rows", "20 columns" per row, and 10,000 characters per table (and in aggregate across a message's tables).
// https://docs.slack.dev/reference/block-kit/blocks/section-block/ (checked 2026-07-27) — a section's text is
// "maximum length is 3000 characters".
const (
	MaxBlocks       = 50
	MaxTableRows    = 100
	MaxTableColumns = 20
	MaxTableChars   = 10000
	MaxSectionText  = 3000
)

// truncationMarker is what a cut looks like. It is never silent, for the reason TruncateMarkdown and
// ThreadReply give: a reader who cannot tell a truncated answer from a complete one has been told something
// false.
const truncationMarker = "… (truncated)"

// MaxMarkdownText is the markdown block's budget, and it is CUMULATIVE ACROSS A PAYLOAD rather than per
// block — three 5,000-character blocks are over the limit even though none of them is. That is why it is
// threaded through the render as a running budget instead of checked at each builder.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/markdown-block/ (checked 2026-07-27) — "The
// cumulative limit for all `markdown` blocks in a single payload is 12,000 characters." The block carries
// `type` and `text`, both required, and nothing else that survives: "The `block_id` is ignored in markdown
// blocks and will not be retained."
const MaxMarkdownText = 12000

// Result is ONE element of the closed union. It is a flat struct rather than four types because it is decoded
// straight from a model's JSON: encoding/json drops fields we do not declare, so structure we never chose
// cannot survive the parse at all. Type is the discriminator and nothing else is trusted.
type Result struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// table + tasks
	Title   string     `json:"title,omitempty"`
	Columns []string   `json:"columns,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`
	Tasks   []Task     `json:"tasks,omitempty"`
	// file_ref
	URL   string `json:"url,omitempty"`
	Label string `json:"label,omitempty"`
	// mention. Who is a CLOSED ENUM (MentionRequester and nothing else), which is why the field is a string
	// the parse keeps rather than an id the render trusts.
	Who string `json:"who,omitempty"`
	// mint is the user id the `<@U…>` token is built from, and it is UNEXPORTED ON PURPOSE: encoding/json
	// cannot populate an unexported field, so no model output can reach it, and no package outside this one
	// can set it either. RenderOutput — the only function that is given the requester — is the only writer.
	// A Result that arrives any other way carries no id and therefore mints nothing.
	mint string
}

// Task is one durable task as this renderer needs it. It is the shape of coordinator.Task{Key, Kind, Title,
// Status, Detail} minus Kind (which has no home in Slack's card) — declared here rather than imported
// because this package is PURE: it has zero internal dependencies today, and importing the coordinator would
// drag a database driver into a wire adapter to borrow five fields.
type Task struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Sources are the places a reader can go to check the task — E22's are the pull request and the Jira
	// ticket a coding run worked from. E21 T6 left this field out with a reason ("no user today"); E22 is the
	// user, so it lands with the same trust boundary every other link in this file has.
	Sources []TaskSource `json:"sources,omitempty"`
}

// TaskSource is one URL source element on a task card.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/task-card-block/ (checked 2026-07-28) —
// `sources` is an "Array of URL source elements", and the page's own example shows the element as
// {"type":"url","url":"https://weather.com/","text":"weather.com"}.
//
// IT IS NOT ACTIONABLE, and that is why it may exist at all: a URL source is a link, so it needs no
// authorization path of its own (unlike a button, which E20 §2 confines to interactions.go). The claim is
// held by a test rather than by this sentence — blocks_test.go's sweep runs over a rendered card carrying
// these and stays green, and the AST scan still finds a mint in exactly one file.
type TaskSource struct {
	URL  string `json:"url"`
	Text string `json:"text,omitempty"`
}

// MaxTaskSources caps how many links one card carries. IT IS OUR NUMBER: the reference prints no limit
// (checked 2026-07-28), and a card with two hundred links is not a card. The cut is silent HERE and only
// here, because a source element has no room for a truncation marker and the sources are corroboration
// rather than the answer — the answer itself is never truncated silently (see truncationMarker).
const MaxTaskSources = 10

// taskStatus is S10's mapping table, and it is EXPLICIT because the two vocabularies do not overlap: ours is
// a free string (the palai.task tool documents `open | in_progress | done | canceled` and the store defaults
// to `open`), Slack's is a fixed four.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/task-card-block/ (checked 2026-07-27) — status
// is one of pending, in_progress, complete, error.
//
// `canceled` maps to error rather than complete on purpose: a cancelled task did not do what it said, and the
// one thing the mapping must never do is report unfinished work as finished.
var taskStatus = map[string]string{
	"open":        "pending",
	"in_progress": "in_progress",
	"done":        "complete",
	"canceled":    "error",
}

// TaskStatus maps one of our statuses onto Slack's. An UNMAPPED status FAILS CLOSED to pending — it is never
// guessed, never string-matched into `complete`, and never passed through. A future status this table has not
// been taught reads as "not done yet", which is the only wrong answer that cannot mislead a reader into
// thinking work finished.
func TaskStatus(status string) string {
	if mapped, ok := taskStatus[status]; ok {
		return mapped
	}
	return "pending"
}

// TaskDisplayMode reports how a run's tasks should be shown: the documented default for one, and the grouped
// view once there is more than one.
//
// CONTRACT: https://docs.slack.dev/reference/methods/chat.startStream/ (checked 2026-07-27) — task_display_mode
// is `timeline` (the default), `plan` or `dense`, and it is an argument of chat.startStream ONLY.
//
// So this decision is applied at the BLOCK layer instead of on the wire field: our blocks travel on
// chat.stopStream (the conservative reading of S12), which has no such argument. One task renders as a bare
// task_card, several as a plan block — which is exactly what the vendor says the two modes do.
func TaskDisplayMode(tasks int) string {
	if tasks > 1 {
		return "plan"
	}
	return "timeline"
}

// RenderOutput splits a run's answer into the two fields chat.stopStream carries: the markdown_text and the
// blocks rendered under it. tasks are the journal's own tasks (task.created/updated.v1, collected by the run
// follower), which is what gives the task card a user today.
//
// THE SPLIT MATTERS: text parts become the markdown, structured parts become blocks, and neither is rendered
// twice. An ordinary prose answer — which is what every real single-step run produces (E08) — comes back as
// exactly the markdown it was, with NO blocks at all, so this task did not change what a normal reply looks
// like. Only an answer that is genuinely typed grows structure.
//
// requesterUserID (E21 T3) is the ONE identity a rendered answer can address: the person whose message birthed
// this run, frozen on the delivery row at enqueue time (000043). It enters HERE, at the only door the reply
// pump uses, and it is stamped onto the mention variant's unexported field — so the id travels with the
// result the model asked for without ever being something the model could name. EMPTY IS FAIL-CLOSED: a
// delivery row written before 000043 carries ”, and its answer goes out with the words and no mention.
func RenderOutput(answer string, tasks []Task, requesterUserID string) (markdown string, blocks json.RawMessage) {
	var text []string
	var structured []Result
	for _, result := range resultsFor(answer, tasks, requesterUserID) {
		if result.Type == ResultText {
			if strings.TrimSpace(result.Text) != "" {
				text = append(text, result.Text)
			}
			continue
		}
		structured = append(structured, result)
	}
	return strings.Join(text, "\n\n"), RenderBlocks(structured)
}

// resultsFor is what a run's answer IS, before either surface decides how to draw it: the closed union as
// parsed, with the one identity a render may address stamped onto a mention variant, and the journal's own
// tasks appended. Shared by RenderOutput and ReplyMessage so the two paths can never disagree about the
// content — only about the blocks.
func resultsFor(answer string, tasks []Task, requesterUserID string) []Result {
	results := ParseResults(answer)
	for i := range results {
		if results[i].Type == ResultMention && results[i].Who == MentionRequester {
			results[i].mint = requesterUserID
		}
	}
	if len(tasks) > 0 {
		results = append(results, Result{Type: ResultTasks, Tasks: tasks})
	}
	return results
}

// ParseResults reads a model's answer as the closed union. It NEVER fails: prose is one text result, and any
// JSON that is not this union — a foreign tag, a malformed variant, a forged Block Kit array — becomes a text
// result carrying its own bytes. That is the "inert text, never passthrough" rule at the parse boundary,
// where it is cheapest: a shape that never becomes a Result can never reach a builder.
func ParseResults(answer string) []Result {
	trimmed := strings.TrimSpace(answer)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return []Result{{Type: ResultText, Text: answer}}
	}
	raws := []json.RawMessage{json.RawMessage(trimmed)}
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &raws); err != nil {
			return []Result{{Type: ResultText, Text: answer}}
		}
	}
	results := make([]Result, 0, len(raws))
	for _, raw := range raws {
		results = append(results, parseResult(raw, answer))
	}
	return results
}

// parseResult decodes ONE union element. whole is the original answer, used when a single top-level object
// turns out not to be ours — the reader should then see the answer as written, not a re-serialized copy.
func parseResult(raw json.RawMessage, whole string) Result {
	var tag struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &tag); err != nil {
		return Result{Type: ResultText, Text: string(raw)}
	}
	switch tag.Type {
	case ResultText, ResultTable, ResultTasks, ResultFileRef, ResultMention:
		var result Result
		if err := json.Unmarshal(raw, &result); err != nil {
			// A known tag with a shape we cannot decode is NOT half-rendered: it falls to inert text like
			// anything else, because a partially-decoded structure is structure we did not verify.
			return Result{Type: ResultText, Text: string(raw)}
		}
		return result
	}
	if strings.TrimSpace(whole) == string(raw) {
		return Result{Type: ResultText, Text: whole}
	}
	return Result{Type: ResultText, Text: string(raw)}
}

// ReplyMessage builds the chat.postMessage body for a run's answer — the OTHER of the two paths a reply
// takes, and the one where the model's markdown was being lost (E21 T6, M13).
//
// THE LOSS WAS MEASURED, not assumed. chat.stopStream already carries prose in `markdown_text`, which is real
// markdown with a 12,000-character budget. chat.postMessage carries it in `text`, which is Slack's own
// narrower mrkdwn: a fenced block loses its language, a header renders as a literal `#`, a table does not
// render and a nested list collapses. So on THIS path the prose becomes a markdown block, and `text` stays
// exactly what it was — with blocks present Slack draws the blocks and uses text as the notification
// fallback, so every push preview reads as it did before.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/markdown-block/ (checked 2026-07-27) — the
// page's own example is a message payload, `{"blocks":[{"type":"markdown","text":"…"}]}`, and its usage note
// says the block exists "when you expect a markdown response from an LLM that can get lost in translation
// rendering in Slack", which is this exact reply.
//
// ThreadReply is CALLED rather than reimplemented: it owns the text field's neutralisation, its truncation
// marker and the thread_ts rule, and a second copy of those would be a second place for them to drift.
//
// THE JOURNAL'S TASKS ARE DELIBERATELY NOT A PARAMETER, and the reason is the failure mode rather than taste.
// A stopStream that Slack answers invalid_blocks on falls back to this call on the next attempt; THIS call
// has no fallback below it, so a rejected body is an answer nobody sees. task_card/plan carry the one shape
// E20 named as UNCONFIRMED (the reference table calls plan's `title` an Object, its example a bare string),
// so they stay on the path that can recover. The plain post renders the model's own answer and keeps the
// task cards as the stream's decoration — which is exactly what it did before this task, minus the lost
// markdown.
func ReplyMessage(channel, threadTS, answer, responseID, requesterUserID string) []byte {
	results := resultsFor(answer, nil, requesterUserID)
	var prose []string
	for _, result := range results {
		if result.Type == ResultText && strings.TrimSpace(result.Text) != "" {
			prose = append(prose, result.Text)
		}
	}
	// The fallback is the prose, or the whole answer when there is none — never empty, because an answer that
	// is all structure would otherwise arrive as a push notification with nothing in it.
	fallback := strings.Join(prose, "\n\n")
	if fallback == "" {
		fallback = answer
	}
	body := ThreadReply(channel, threadTS, fallback, responseID)
	blocks := renderBlocks(results, true)
	if len(blocks) == 0 {
		return body
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body // unreachable: ThreadReply marshals an object
	}
	fields["blocks"] = blocks
	raw, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return raw
}

// RenderBlocks is the TOTAL render for the chat.stopStream path: every Result becomes blocks, an unknown one
// becomes inert text, and the array is held to the documented ceiling with a visible marker. nil when there
// is nothing to render, which is what keeps a plain answer a plain answer.
func RenderBlocks(results []Result) json.RawMessage { return renderBlocks(results, false) }

// renderBlocks is that render with the SURFACE named, because the two paths do not carry the same blocks.
//
// markdown selects the chat.postMessage surface, where a text result becomes a markdown block. It is FALSE
// for chat.stopStream and that is M20(d) fail-closed: no published page says stopStream's `blocks` array
// accepts a markdown block. A vendor's silence is not a design freedom (E20 S12's rule), so the streaming
// path keeps the section it has always sent until §6 leg 1 measures the other. The cost is named rather than
// hidden: a run that ends by STREAMING gets the older render, which is visible, inconsistent and deliberate.
func renderBlocks(results []Result, markdown bool) json.RawMessage {
	blocks := make([]any, 0, len(results))
	budget := MaxMarkdownText
	truncated := false
	for _, result := range results {
		// The cumulative budget is checked HERE, before the builder, so an exhausted payload stops rather
		// than emitting a run of blocks that are nothing but truncation markers.
		if markdown && result.Type == ResultText && budget <= 0 {
			truncated = true
			break
		}
		for _, block := range renderResult(result, markdown, &budget) {
			if len(blocks) >= MaxBlocks-1 {
				truncated = true
				break
			}
			blocks = append(blocks, block)
		}
		if truncated {
			break
		}
	}
	if truncated {
		blocks = append(blocks, section("_"+truncationMarker+"_"))
	}
	if len(blocks) == 0 {
		return nil
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return nil // unreachable: every value above is a map/slice of strings
	}
	return raw
}

// renderResult is the union's one dispatch. The default arm is the security-relevant one: it renders the
// variant's own JSON as TEXT, so an unknown tag is something a human reads rather than something Slack draws.
//
// budget is the payload's remaining markdown allowance and is spent by markdownBlock alone; every other
// builder here has its own documented ceiling.
func renderResult(result Result, markdown bool, budget *int) []any {
	switch result.Type {
	case ResultText:
		if markdown {
			if strings.TrimSpace(result.Text) == "" {
				return nil // nothing to draw; a markdown block with an empty text field is invalid_blocks
			}
			return []any{markdownBlock(result.Text, budget)}
		}
		return []any{section(result.Text)}
	case ResultTable:
		return []any{tableBlock(result)}
	case ResultTasks:
		return taskBlocks(result)
	case ResultFileRef:
		return []any{fileRefBlock(result)}
	case ResultMention:
		// A `who` we did not choose selects NOBODY, and it falls through to the inert arm below rather than
		// rendering as text-minus-the-mention: a model that wrote a user id into this field tried to pick a
		// person, and the honest thing to show the reader is what it wrote. `requester` with an empty id is
		// the OTHER case and it does render (mrkdwnSection mints nothing) — that is a row from before
		// 000043, not an attempt.
		if result.Who == MentionRequester {
			return []any{mrkdwnSection(result.mint, result.Text)}
		}
	}
	inert, err := json.Marshal(result)
	if err != nil {
		return nil
	}
	return []any{section(string(inert))}
}

// section is the one text block this renderer emits.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/section-block/ (checked 2026-07-27) — a section
// carries a text object, and mrkdwn text is limited to 3000 characters.
//
// Every string that reaches Slack through a BLOCK passes through here or through cell(): blocks do not route
// through TruncateMarkdown or ThreadReply, which are where the text path's broadcast defence lives, so this
// calls the SAME NeutralizeBroadcasts rather than growing a third copy of the rule.
func section(text string) any { return mrkdwnSection("", text) }

// mrkdwnSection builds that block with an OPTIONAL minted mention in front of the model's words, and THE
// ORDER IS THE WHOLE SECURITY PROPERTY (plan §2, M19): the model's text is defused FIRST, our token is added
// SECOND. Reversed, NeutralizeBroadcasts would escape our own `<@U…>` into characters; merged into one pass,
// a model could write the token and have it survive. So they are two steps and this comment says which is
// which.
//
// mentionUserID is ours or it is empty — see Result.mint, which no model output and no other package can
// reach. Empty mints nothing, which is what makes a delivery row from before 000043 send its words rather
// than an invented address.
//
// The truncation cuts the TAIL, so the token cannot be halved into a live `<@` fragment.
func mrkdwnSection(mentionUserID, text string) any {
	text = NeutralizeBroadcasts(text)
	if mentionUserID != "" {
		text = "<@" + mentionUserID + "> " + text
	}
	if runes := []rune(text); len(runes) > MaxSectionText {
		text = string(runes[:MaxSectionText-len([]rune(truncationMarker))]) + truncationMarker
	}
	if text == "" {
		text = "_(no text)_" // a section with an empty text field is invalid_blocks
	}
	return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
}

// markdownBlock is the fidelity fix itself: the model's markdown handed to Slack AS markdown.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/markdown-block/ (checked 2026-07-27) — `type`
// and `text` are the block's fields and both are required; block_id is not sent because the same page says it
// "is ignored in markdown blocks and will not be retained". Supported syntax includes headers, block quotes,
// tables, task lists, nested lists and CODE BLOCKS WITH SYNTAX HIGHLIGHTING — the four this reply was losing.
// The page's own reason for the block is this exact case: "a markdown response from an LLM that can get lost
// in translation rendering in Slack".
//
// THE DEFENCE IS UNCHANGED, AND THAT IS THE POINT (plan §2): a richer render is not a weaker one, so the text
// goes through the SAME NeutralizeBroadcasts as every other builder in this file rather than growing a
// dialect-specific copy of the rule. The cut takes the TAIL, so an escape can never be halved back into a
// live `<`, and it is never silent.
//
// budget is the payload's REMAINING cumulative allowance and this is the only function that spends it.
func markdownBlock(text string, budget *int) any {
	text = NeutralizeBroadcasts(text)
	runes := []rune(text)
	if len(runes) > *budget {
		marker := []rune(truncationMarker)
		keep := max(*budget-len(marker), 0)
		runes = append(runes[:keep:keep], marker...)
		text = string(runes)
	}
	*budget -= len(runes)
	return map[string]any{"type": "markdown", "text": text}
}

// tableBlock renders the test-results table.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/table-block/ (checked 2026-07-27) — `rows` is
// an array of rows of cells; a cell is rich_text, raw_text or raw_number; at most 100 rows, 20 columns per
// row, 10,000 characters per table.
//
// The columns become the header row, which is how the vendor's own example shows headers.
func tableBlock(result Result) any {
	rows := result.Rows
	if len(result.Columns) > 0 {
		rows = append([][]string{result.Columns}, rows...)
	}
	out := make([]any, 0, len(rows))
	budget := MaxTableChars
	truncated := false
	for _, row := range rows {
		// Both ceilings are checked HERE, at the top, so `truncated` is only ever set while rows remain: a
		// table that lands exactly on a limit was not cut and must not claim it was.
		if len(out) >= MaxTableRows-1 || budget <= 0 {
			truncated = true
			break
		}
		if len(row) > MaxTableColumns {
			row = row[:MaxTableColumns]
			truncated = true
		}
		cells := make([]any, 0, len(row))
		for _, text := range row {
			text = NeutralizeBroadcasts(text)
			if len([]rune(text)) > budget {
				text = string([]rune(text)[:max(budget, 0)])
				truncated = true
			}
			budget -= len([]rune(text))
			cells = append(cells, cell(text))
		}
		out = append(out, cells)
	}
	if truncated {
		marker := make([]any, 0, MaxTableColumns)
		width := 1
		if len(out) > 0 {
			if first, ok := out[0].([]any); ok && len(first) > 0 {
				width = len(first)
			}
		}
		for i := 0; i < width; i++ {
			text := ""
			if i == 0 {
				text = truncationMarker
			}
			marker = append(marker, map[string]any{"type": "raw_text", "text": text})
		}
		out = append(out, marker)
	}
	return map[string]any{"type": "table", "rows": out}
}

// taskBlocks renders the journal's tasks. ONE task is a bare card; SEVERAL are grouped in a plan block, which
// is TaskDisplayMode's decision applied where our output actually lands (see that function).
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/task-card-block/ and
// https://docs.slack.dev/reference/block-kit/blocks/plan-block/ (both checked 2026-07-27) — a task_card
// requires task_id and title and optionally carries status, details, output and sources; a plan requires a
// title and carries a `tasks` array of task cards.
//
// UNCONFIRMED, and named rather than assumed: the reference table calls plan's `title` an Object while the
// page's own example shows a bare string, and task_card's example shows a bare string too. This follows the
// EXAMPLES, because a copy-pasteable example is the stronger evidence — and the cost of being wrong is
// bounded and known: Slack answers invalid_blocks, the stop fails, and the reply pump's next attempt posts
// the answer as a plain message (slack_reply.go). The answer is never lost, only undecorated. §6 leg 1
// measures it.
func taskBlocks(result Result) []any {
	cards := make([]any, 0, len(result.Tasks))
	for _, task := range result.Tasks {
		title := NeutralizeBroadcasts(task.Title)
		if title == "" {
			title = NeutralizeBroadcasts(task.ID)
		}
		if title == "" {
			continue // a card without a title is invalid_blocks, and an untitled task says nothing anyway
		}
		card := map[string]any{
			"type":    "task_card",
			"task_id": task.ID,
			"title":   title,
			"status":  TaskStatus(task.Status),
		}
		if detail := NeutralizeBroadcasts(task.Detail); detail != "" {
			card["details"] = richText(detail)
		}
		if sources := taskSources(task.Sources); len(sources) > 0 {
			card["sources"] = sources
		}
		cards = append(cards, card)
	}
	if len(cards) == 0 {
		return nil
	}
	if TaskDisplayMode(len(cards)) == "timeline" {
		return cards
	}
	title := NeutralizeBroadcasts(result.Title)
	if title == "" {
		title = "Tasks"
	}
	return []any{map[string]any{"type": "plan", "title": title, "tasks": cards}}
}

// cell types one table cell by WHAT IT IS, which is M14's whole (small, real) win: a cell that is a number is
// declared as one, and Slack then right-aligns the column and sorts it numerically instead of alphabetically.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/table-block/ (checked 2026-07-27) — cells are
// rich_text, raw_text or raw_number, and "raw_number support numeric values"; "if a column contains cells all
// of type raw_number, a numeric sort will be performed instead of the default alphabetical sort".
//
// ONLY `type` AND `text` ARE SENT, and that is a decision with two reasons rather than an omission. The table
// block's own page prints NO schema for raw_number. The sibling
// https://docs.slack.dev/reference/block-kit/blocks/data-table-block/ (same date) does, and it gives the cell
// a `value` of JSON type number beside its `text` — but it is a DIFFERENT BLOCK and its schema names no
// required set, so "raw_number needs a value" is an inference across two pages, not a contract. The second
// reason is ours and it is not a preference: a `value` KEY IS WHAT OUR OWN ACTIONABLE-ELEMENT SWEEP LOOKS FOR
// (tests/uat/evidence.go SweepActionableElements, blocks_test.go sweepActionable — a button's `value` is the
// payload it dispatches). A numeric cell carrying one would register as a forged interaction in every
// evidence bundle, and the only way to make that green again is to weaken the sweep. A right-aligned column
// is not worth a hole in the defence this renderer exists for.
//
// SO THE UNCONFIRMED IS NAMED: if a live leg answers invalid_blocks on a numeric table, `value` is the
// suspect — and adding it is a question for the sweep before it is a question for the renderer.
//
// raw_text stays the DEFAULT and that is a security choice as much as a simplicity one: the documentation
// describes raw_text as "basic text characters" while rich_text is the type that carries mentions and links.
// A model's bytes go in the type with the least interpretation, and they are defused on the way in regardless.
//
// The decode IS the type test: encoding/json accepts exactly the JSON number grammar, so `1.5s`, `NaN`,
// `0x1f`, `007 ` and a quoted `"42"` all stay raw_text rather than being coerced into numbers they are not.
func cell(text string) map[string]any {
	var number json.Number
	if err := json.Unmarshal([]byte(text), &number); err == nil {
		return map[string]any{"type": "raw_number", "text": text}
	}
	return map[string]any{"type": "raw_text", "text": text}
}

// richText is the rich_text object a task card's details field takes, in the shape the task-card reference's
// own example shows: one rich_text_section holding one text element.
func richText(text string) any {
	if runes := []rune(text); len(runes) > MaxSectionText {
		text = string(runes[:MaxSectionText-len([]rune(truncationMarker))]) + truncationMarker
	}
	return map[string]any{
		"type": "rich_text",
		"elements": []any{map[string]any{
			"type":     "rich_text_section",
			"elements": []any{map[string]any{"type": "text", "text": text}},
		}},
	}
}

// fileRefBlock renders an artifact as a LINK — the honest form of S14's scope decision. There is no file
// upload here and no file block: the file block is remote-source only, and uploading needs files:write.
//
// A LINK IS A PLACE A MODEL GETS TO SEND A READER, so the URL is validated at this trust boundary rather
// than trusted: anything that is not plain http(s), or that carries a character which would break out of
// Slack's `<url|label>` syntax, renders as inert text instead of a clickable target.
//
// SOURCE for the syntax: https://docs.slack.dev/messaging/formatting-message-text/ (checked 2026-07-27).
// E22 T5 CHANGES THE DELIVERY, NOT THIS RENDER. An artifact a run produced now also ARRIVES in the thread as
// a real file (extensions/slack_upload.go resolves what this link names and uploads the bytes), and the block
// below is byte-identical to what it was before that: same link, same validation, same inert-text fallback.
// A reader sees the link they always saw, plus the file it points at.
func fileRefBlock(result Result) any {
	label := NeutralizeBroadcasts(result.Label)
	if label == "" {
		label = "artifact"
	}
	label = strings.NewReplacer("<", "&lt;", ">", "&gt;", "|", "&#124;").Replace(label)
	if !linkableURL(result.URL) {
		return section(label + ": " + NeutralizeBroadcasts(result.URL))
	}
	return section("<" + result.URL + "|" + label + ">")
}

// linkableURL is the trust boundary every model-supplied link in this file crosses: plain http(s), a real
// host, and no character that would break out of Slack's `<url|label>` syntax. Shared by fileRefBlock and
// taskSources rather than copied, because two copies of a security predicate is one copy that will be fixed.
func linkableURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" &&
		!strings.ContainsAny(raw, "<>| \t\n")
}

// taskSources builds the card's URL source elements. A source whose URL does not survive linkableURL is
// DROPPED rather than rendered as text: unlike a file_ref — which IS the answer, so it falls back to inert
// text — a source is corroboration hanging off a card that already says what happened, and a card carrying
// "here is where to check: javascript:…" as prose is worse than a card carrying nothing.
//
// The text is neutralised like every other string this renderer emits, and defaults to the URL's host when a
// model supplied none: a bare link with no words is still a place to click, and the host is the honest label
// for it.
func taskSources(sources []TaskSource) []any {
	out := make([]any, 0, len(sources))
	for _, src := range sources {
		if len(out) >= MaxTaskSources {
			break
		}
		if !linkableURL(src.URL) {
			continue
		}
		text := NeutralizeBroadcasts(src.Text)
		if text == "" {
			parsed, _ := url.Parse(src.URL)
			text = parsed.Host
		}
		out = append(out, map[string]any{"type": "url", "url": src.URL, "text": text})
	}
	return out
}

// ---------------------------------------------------------------------------------------------------
// E23 T4 — THE APPROVAL SCREEN'S NON-ACTIONABLE HALF.
//
// Everything below builds blocks that DO NOTHING when a human touches them: a table of arguments and an
// alert. They live in this file rather than in interactions.go on purpose — interactions.go is the single
// mint of actionable elements, and the AST scan in blocks_test.go is what keeps that true, so a builder
// that is not a mint belongs on this side of the line and must stay free of `action_id` and `value`.
// ---------------------------------------------------------------------------------------------------

// AlertLevels are the five the vendor documents; anything else falls to `default` rather than being sent
// as a level Slack does not know.
//
// CONTRACT (§3.5 P12, inherited from E21 M16): the alert block carries `text` (max 200 characters) and
// `level` ∈ default|info|warning|error|success, and — the sentence that matters — "Alert blocks are
// currently only supported in modals." E21 recorded that as "structurally dead" and it was RIGHT: this
// tree had no modal. E23 T4 opens one, so the reason for the rejection is gone and the rejection with it.
var AlertLevels = map[string]bool{"default": true, "info": true, "warning": true, "error": true, "success": true}

// MaxAlertText is the alert block's own budget (P12). Cut visibly, like every other limit in this file.
const MaxAlertText = 200

// alertBlock is the WARNING a modal is allowed to carry — "these arguments were cut", "this approval
// expires in twelve minutes". Both are things a human deciding needs and neither is a thing they should
// have to infer from an absence.
func alertBlock(level, text string) any {
	if !AlertLevels[level] {
		level = "default"
	}
	text = NeutralizeBroadcasts(text)
	if runes := []rune(text); len(runes) > MaxAlertText {
		text = string(runes[:MaxAlertText-len([]rune(truncationMarker))]) + truncationMarker
	}
	return map[string]any{"type": "alert", "level": level, "text": text}
}

// ApprovalArgument is one row of the approval screen's argument table: the argument's name and the exact
// JSON encoding of its value.
//
// THE VALUE IS ITS JSON ENCODING AND NOT ITS TEXT, which is the difference between a screen that describes
// a call and a screen that shows it. A string "3" renders as `"3"` and a number 3 renders as `3`, so a
// human reading the table is reading the bytes the broker will send — which is the entire claim an
// approval bound to tool_calls.request_hash is making.
type ApprovalArgument struct {
	Name  string
	Value string
}

// ApprovalArgumentRows is the CHANNEL message's view: one row per top-level argument, nested values
// compacted into a single cell. Arguments that are not a JSON object — an array, a scalar, bytes that do
// not parse — render as one row named `(arguments)` carrying them verbatim, because a strange call is
// exactly the call a human should be shown rather than shielded from.
func ApprovalArgumentRows(arguments []byte) []ApprovalArgument {
	decoded, err := decodeApprovalArguments(arguments)
	if err != nil {
		return []ApprovalArgument{{Name: "(arguments)", Value: NeutralizeBroadcasts(string(arguments))}}
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return []ApprovalArgument{{Name: "(arguments)", Value: encodeArgumentValue(decoded)}}
	}
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	sort.Strings(names) // the canonical order is the same one encoding/json gives the dump: sorted keys
	out := make([]ApprovalArgument, 0, len(names))
	for _, name := range names {
		out = append(out, ApprovalArgument{
			Name:  NeutralizeBroadcasts(name),
			Value: encodeArgumentValue(object[name]),
		})
	}
	return out
}

// ApprovalArgumentLeaves is the MODAL's view: the same document flattened to one row per LEAF, addressed
// by the path that reaches it (`fields.assignee`, `fields.labels[0]`). It is what the third button buys —
// the channel gets a summary that fits in a thread, and the human who wants the whole thing opens it.
//
// An empty object or array is a leaf: `{}` says something different from a key that is not there at all.
func ApprovalArgumentLeaves(arguments []byte) []ApprovalArgument {
	decoded, err := decodeApprovalArguments(arguments)
	if err != nil {
		return []ApprovalArgument{{Name: "(arguments)", Value: NeutralizeBroadcasts(string(arguments))}}
	}
	var out []ApprovalArgument
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch v := node.(type) {
		case map[string]any:
			if len(v) == 0 {
				out = append(out, ApprovalArgument{Name: path, Value: "{}"})
				return
			}
			names := make([]string, 0, len(v))
			for name := range v {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				child := NeutralizeBroadcasts(name)
				if path != "" {
					child = path + "." + child
				}
				walk(child, v[name])
			}
		case []any:
			if len(v) == 0 {
				out = append(out, ApprovalArgument{Name: path, Value: "[]"})
				return
			}
			for i, el := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), el)
			}
		default:
			if path == "" {
				path = "(arguments)"
			}
			out = append(out, ApprovalArgument{Name: path, Value: encodeArgumentValue(node)})
		}
	}
	walk("", decoded)
	return out
}

// encodeArgumentValue marshals one value back to its JSON form with the broadcast escape applied to every
// string inside it. Marshalling can only fail on values encoding/json cannot represent, and every value
// here came OUT of encoding/json, so the fallback is unreachable in practice and honest rather than silent
// if it ever is not.
func encodeArgumentValue(v any) string {
	raw, err := encodeCanonicalJSON(neutralizeJSON(v), "")
	if err != nil {
		return "(unrenderable)"
	}
	return raw
}

// approvalArgumentTable renders the rows as the vendor's table block, reusing tableBlock so the argument
// screen inherits E20's limits rather than restating them: 100 rows, 20 columns, 10,000 characters, each
// one a TRUNCATION POINT with a visible marker row (§3.5 P13). The header names the two columns.
//
// The cells go through cell(), which means a value that is a bare JSON number lands in `raw_number` — the
// vendor's other non-interactive cell type — rather than `raw_text`. That is E20's decision and it is not
// re-made here: forking a helper that decides how a model's bytes are typed would be two copies of a
// security-relevant rule, and the second copy is the one that never gets fixed.
func approvalArgumentTable(args []ApprovalArgument) any {
	rows := make([][]string, 0, len(args))
	for _, arg := range args {
		rows = append(rows, []string{arg.Name, arg.Value})
	}
	return tableBlock(Result{Type: ResultTable, Columns: []string{"argument", "value"}, Rows: rows})
}
