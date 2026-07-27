package slack

import (
	"encoding/json"
	"net/url"
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
}

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
	for _, result := range ParseResults(answer) {
		if result.Type == ResultText {
			if strings.TrimSpace(result.Text) != "" {
				text = append(text, result.Text)
			}
			continue
		}
		if result.Type == ResultMention && result.Who == MentionRequester {
			result.mint = requesterUserID
		}
		structured = append(structured, result)
	}
	if len(tasks) > 0 {
		structured = append(structured, Result{Type: ResultTasks, Tasks: tasks})
	}
	return strings.Join(text, "\n\n"), RenderBlocks(structured)
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

// RenderBlocks is the TOTAL render: every Result becomes blocks, an unknown one becomes inert text, and the
// array is held to the documented ceiling with a visible marker. nil when there is nothing to render, which
// is what keeps a plain answer a plain answer.
func RenderBlocks(results []Result) json.RawMessage {
	blocks := make([]any, 0, len(results))
	truncated := false
	for _, result := range results {
		for _, block := range renderResult(result) {
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
func renderResult(result Result) []any {
	switch result.Type {
	case ResultText:
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

// tableBlock renders the test-results table.
//
// CONTRACT: https://docs.slack.dev/reference/block-kit/blocks/table-block/ (checked 2026-07-27) — `rows` is
// an array of rows of cells; a cell is rich_text, raw_text or raw_number; at most 100 rows, 20 columns per
// row, 10,000 characters per table.
//
// CELLS ARE raw_text, and that is a security choice as much as a simplicity one: the documentation describes
// raw_text as "basic text characters" while rich_text is the type that carries mentions and links. A model's
// bytes go in the type with the least interpretation, and they are defused on the way in regardless.
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
			cells = append(cells, map[string]any{"type": "raw_text", "text": text})
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
func fileRefBlock(result Result) any {
	label := NeutralizeBroadcasts(result.Label)
	if label == "" {
		label = "artifact"
	}
	label = strings.NewReplacer("<", "&lt;", ">", "&gt;", "|", "&#124;").Replace(label)
	parsed, err := url.Parse(result.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		strings.ContainsAny(result.URL, "<>| \t\n") {
		return section(label + ": " + NeutralizeBroadcasts(result.URL))
	}
	return section("<" + result.URL + "|" + label + ">")
}
