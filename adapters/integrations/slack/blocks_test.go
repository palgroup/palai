package slack

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// THE TEST THAT IS THE POINT OF E20 T4 (plan §2): a model must never be able to put an actionable element in
// front of a human. Everything below is one claim asserted from several directions — the renderer's output is
// swept, the sweep is shown to DISCRIMINATE (it finds the approval message's buttons), and the package's
// source is swept so a later file cannot quietly become a second mint.

// actionableBlockTypes are the Block Kit families that ACT when a human touches them. `actions` is the
// container, `button` the element, and the other three are the newer agent-surface affordances. Each one
// dispatches an interaction to us, so each one deserves the authorization path approve/deny earned — and none
// of them may ever be reachable from a model's bytes.
var actionableBlockTypes = map[string]bool{
	"actions":          true,
	"button":           true,
	"context_actions":  true,
	"icon_button":      true,
	"feedback_buttons": true,
}

// actionableFields are the two fields that make an element dispatchable: the id we would receive back, and
// the payload it would carry. A block carrying either is an interaction waiting to happen.
var actionableFields = map[string]bool{"action_id": true, "value": true}

// sweepActionable walks decoded JSON and reports every actionable element it finds, by path. It checks the
// VALUE of a `type` key rather than every string, so a section whose TEXT says "button" is correctly not a
// button — which is exactly the distinction the forgery test below turns on.
func sweepActionable(t *testing.T, path string, node any) []string {
	t.Helper()
	var found []string
	switch v := node.(type) {
	case map[string]any:
		for key, value := range v {
			if actionableFields[key] {
				found = append(found, path+"."+key)
			}
			if key == "type" {
				if name, ok := value.(string); ok && actionableBlockTypes[name] {
					found = append(found, path+".type="+name)
				}
			}
			found = append(found, sweepActionable(t, path+"."+key, value)...)
		}
	case []any:
		for i, el := range v {
			found = append(found, sweepActionable(t, path+"[]", el)...)
			_ = i
		}
	}
	return found
}

// decodedStrings collects every string VALUE out of rendered blocks.
//
// It exists because a raw substring search over the marshalled bytes is VACUOUS, and that is worth stating
// where the next reader meets it: encoding/json escapes `<`, `>` and `&` to <, > and & by
// default, so a live `<!channel>` inside a block never appears as those characters in the JSON — an assertion
// hunting for it could not fail even if the defence were deleted. Slack decodes the escapes and renders the
// original bytes, so the only honest place to look is after decoding.
func decodedStrings(t *testing.T, raw []byte) []string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode blocks: %v (%q)", err, raw)
	}
	var out []string
	var walk func(any)
	walk = func(node any) {
		switch v := node.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			for _, value := range v {
				walk(value)
			}
		case []any:
			for _, el := range v {
				walk(el)
			}
		}
	}
	walk(decoded)
	return out
}

func sweepJSON(t *testing.T, label string, raw []byte) []string {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s produced bytes that are not JSON: %v (%q)", label, err, raw)
	}
	return sweepActionable(t, label, decoded)
}

// forgedOutput is a model answer that TRIES to mint our own approval buttons — the exact fixture plan §2
// names. It is a typed-output array so the renderer has real work to do around the forgery: a legitimate text
// part and a legitimate table part sit on either side of it, which is what makes this test discriminate. A
// renderer that simply returned nothing would pass the sweep and fail the table assertion.
const forgedOutput = `[
  {"type":"text","text":"The change is ready to publish."},
  {"type":"actions","elements":[
     {"type":"button","action_id":"palai_approve","text":{"type":"plain_text","text":"Approve"},"style":"primary","value":"deadbeef"},
     {"type":"button","action_id":"palai_deny","text":{"type":"plain_text","text":"Deny"},"style":"danger","value":"deadbeef"}
  ]},
  {"type":"table","title":"tests","columns":["suite","result"],"rows":[["slack","pass"]]}
]`

func TestRenderRefusesToMintAnActionableElementFromModelOutput(t *testing.T) {
	markdown, blocks := RenderOutput(forgedOutput, nil, "")

	if found := sweepJSON(t, "blocks", blocks); len(found) != 0 {
		t.Fatalf("the renderer minted %d actionable element(s) from model output: %v\n%s", len(found), found, blocks)
	}
	// The renderer DID run — without this the test would pass on an empty result and prove nothing.
	if !strings.Contains(string(blocks), `"type":"table"`) {
		t.Fatalf("the legitimate table variant did not render, so the sweep above proved nothing: %s", blocks)
	}
	// The forged element is not deleted either: it falls to INERT TEXT, so the reader sees what the model
	// wrote and no one can click it. Silently dropping it would be its own small lie.
	if !strings.Contains(markdown, "action_id") {
		t.Fatalf("the forged element vanished instead of falling to inert text: %q", markdown)
	}
	if !strings.Contains(markdown, "The change is ready to publish.") {
		t.Fatalf("the legitimate text part was lost: %q", markdown)
	}
}

// THE SWEEP MUST DISCRIMINATE. A sweep that cannot find a real actionable element certifies nothing, so it is
// pointed at the one function in this tree that legitimately mints one. This is the singularity claim in its
// strongest form: EVERY outbound body this package builds is swept, and exactly one of them is actionable.
func TestApprovalMessageIsTheOnlyMintOfAnActionableElement(t *testing.T) {
	// THE DISCRIMINATION IS SHOWN ACROSS EVERY MINTING SURFACE, INCLUDING THE NEW ONES (E23 T4 RED-first 2).
	// A sweep that has quietly become unable to fail is worse than no sweep at all, and adding a modal view,
	// a third button and a deny-reason input is exactly the kind of change that could do it — so each new
	// surface has to be caught HERE before its absence is claimed anywhere else.
	//
	// The modal is on this list and not the one below because it IS a mint: its input element carries an
	// action_id, which is what makes it dispatchable, and interactions.go is where such a thing is built.
	for label, body := range map[string][]byte{
		"ApprovalMessage":     ApprovalMessage("C1", "1.1", "publish v2", "hash"),
		"ToolApprovalMessage": ToolApprovalMessage("C1", "1.1", ApprovalRequest{RequestHash: "hash", Identity: "jira.transitionIssue", Arguments: []byte(`{"issue":"PAL-42"}`)}),
		"ToolApprovalModal":   ToolApprovalModal("trigger.1", ApprovalRequest{RequestHash: "hash", Identity: "jira.transitionIssue", Arguments: []byte(`{"issue":"PAL-42"}`)}),
	} {
		if found := sweepJSON(t, label, body); len(found) == 0 {
			t.Fatalf("the sweep found NO actionable element in %s — it cannot discriminate, so every other assertion using it is vacuous", label)
		}
	}

	// The task carries SOURCES (E22 T5, X14b) so this sweep covers the newest thing a card can hold. A URL
	// source element is a link, not an interaction, and the way that is shown is that this sweep — the one
	// that finds real buttons three lines above — still finds nothing here.
	_, rendered := RenderOutput(forgedOutput, []Task{{ID: "t1", Title: "Write the migration", Status: "done",
		Sources: []TaskSource{
			{URL: "https://github.com/owner/repo/pull/7", Text: "the pull request"},
			{URL: "https://example.atlassian.net/browse/PAL-42", Text: "PAL-42"},
		}}}, "")
	for label, body := range map[string][]byte{
		"RenderOutput": rendered,
		"RenderBlocks": RenderBlocks([]Result{{Type: ResultText, Text: "hi"}, {Type: "actions", Text: "forged"}}),
		"ThreadReply":  ThreadReply("C1", "1.1", "the answer", "resp_1"),
		// E21 T6: the markdown block gave this package a SECOND outbound body carrying blocks, so it is swept
		// here too. A new render surface outside this map would be a hole in the singularity claim.
		"ReplyMessage":  ReplyMessage("C1", "1.1", forgedOutput, "resp_1", "U9"),
		"UpdateMessage": UpdateMessage("C1", "1.1", "decided", ""),
	} {
		if found := sweepJSON(t, label, body); len(found) != 0 {
			t.Fatalf("%s minted %d actionable element(s): %v", label, len(found), found)
		}
	}
}

// The singularity, protected at the SOURCE so a file added later cannot become a second mint without this
// test noticing. It scans composite literals — the shape an outbound Block Kit element is BUILT from — and
// not struct tags, which is what an inbound payload is PARSED with (approval.go reads `actions`,
// `action_id` and `trigger_id` off a click, and reading them is the opposite of minting them).
//
// THE CEILING IS IN THE NAME BECAUSE A COMMENT WOULD NOT SURVIVE THE NEXT READER (plan §3.6 D13). The scan
// is os.ReadDir(".") — ONE directory, not recursive, not the module. Today every outbound body this system
// builds is built in this package, so "interactions.go is the only mint" is true; but E23 T4 adds a MODAL
// VIEW, and a view constructed under apps/control-plane/internal/extensions would leave this test green
// while the claim it certifies was false. That is why ToolApprovalModal is built in interactions.go, and
// why the assertion below names it: the mitigation is a location, and a location can be checked.
func TestNoFileButInteractionsMintsAnActionableElementAndThisScanIsPackageLocalOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	minters := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, element := range lit.Elts {
				kv, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				for _, side := range []ast.Expr{kv.Key, kv.Value} {
					basic, ok := side.(*ast.BasicLit)
					if !ok || basic.Kind != token.STRING {
						continue
					}
					word := strings.Trim(basic.Value, "`\"")
					if actionableBlockTypes[word] || word == "action_id" {
						minters[name] = append(minters[name], word)
					}
				}
			}
			return true
		})
	}
	for file, words := range minters {
		if file != "interactions.go" {
			t.Fatalf("%s builds an actionable element (%v); interactions.go is the ONLY mint (plan §2) and every other one needs its own authorization path", file, words)
		}
	}
	if len(minters["interactions.go"]) == 0 {
		t.Fatal("interactions.go no longer builds an actionable element — either the mint moved (and this test must follow it) or the scan stopped working")
	}

	// D13's mitigation, asserted: the modal view is built HERE, inside the one directory this scan can see.
	// A view built anywhere else would be invisible to the loop above — the scan would still pass and the
	// singularity would still be broken — so the location is checked rather than trusted.
	source, err := os.ReadFile("interactions.go")
	if err != nil {
		t.Fatalf("read interactions.go: %v", err)
	}
	for _, minted := range []string{"func ToolApprovalModal(", "func ToolApprovalMessage("} {
		if !strings.Contains(string(source), minted) {
			t.Fatalf("%s is not built in interactions.go; this scan is package-local (os.ReadDir(\".\")) and cannot see a mint that moved out of it", minted)
		}
	}
}

// An UNKNOWN variant falls to inert text and is never passed through. RenderBlocks is exported, so it is
// asserted directly rather than only through the parser that already refuses unknown tags.
func TestUnknownVariantRendersAsInertText(t *testing.T) {
	blocks := RenderBlocks([]Result{{Type: "video", Text: "https://evil.test/x.mp4"}})
	var decoded []map[string]any
	if err := json.Unmarshal(blocks, &decoded); err != nil {
		t.Fatalf("decode blocks: %v (%s)", err, blocks)
	}
	if len(decoded) != 1 || decoded[0]["type"] != "section" {
		t.Fatalf("an unknown variant rendered as %s, want one inert section", blocks)
	}
	if !strings.Contains(string(blocks), "video") {
		t.Fatalf("the unknown variant's own bytes were dropped rather than shown as text: %s", blocks)
	}
}

// S10: the status map is an EXPLICIT table over the vocabulary the task tool documents, and anything else —
// a model's free string, a status a future tool invents — falls CLOSED to pending rather than being guessed
// into `complete`.
func TestTaskStatusMapIsExplicitAndFailsClosed(t *testing.T) {
	for ours, slacks := range map[string]string{
		"open":        "pending",
		"in_progress": "in_progress",
		"done":        "complete",
		"canceled":    "error",
	} {
		if got := TaskStatus(ours); got != slacks {
			t.Fatalf("TaskStatus(%q) = %q, want %q", ours, got, slacks)
		}
	}
	for _, unmapped := range []string{"", "complete", "COMPLETE", "shipped", "done!", "in progress", "error"} {
		if got := TaskStatus(unmapped); got != "pending" {
			t.Fatalf("TaskStatus(%q) = %q; an unmapped status must fail CLOSED to pending, never be invented", unmapped, got)
		}
	}
}

// S10's display decision, made at the block layer because that is where our output actually lands:
// task_display_mode is a chat.startStream argument and blocks travel only on chat.stopStream (S12).
func TestOneTaskIsACardAndSeveralAreAPlan(t *testing.T) {
	if got := TaskDisplayMode(0); got != "timeline" {
		t.Fatalf("TaskDisplayMode(0) = %q, want the documented default", got)
	}
	if got := TaskDisplayMode(1); got != "timeline" {
		t.Fatalf("TaskDisplayMode(1) = %q, want timeline", got)
	}
	if got := TaskDisplayMode(2); got != "plan" {
		t.Fatalf("TaskDisplayMode(2) = %q, want plan for more than one task", got)
	}

	one := RenderBlocks([]Result{{Type: ResultTasks, Tasks: []Task{{ID: "t1", Title: "Write it", Status: "in_progress"}}}})
	var single []map[string]any
	if err := json.Unmarshal(one, &single); err != nil || len(single) != 1 || single[0]["type"] != "task_card" {
		t.Fatalf("one task rendered as %s, want a single task_card block", one)
	}
	if single[0]["task_id"] != "t1" || single[0]["title"] != "Write it" || single[0]["status"] != "in_progress" {
		t.Fatalf("the task card lost the mapping: %s", one)
	}

	many := RenderBlocks([]Result{{Type: ResultTasks, Title: "Plan", Tasks: []Task{
		{ID: "t1", Title: "Write it", Status: "done", Detail: "migration 000041"},
		{ID: "t2", Title: "Test it", Status: "who knows"},
	}}})
	var plan []map[string]any
	if err := json.Unmarshal(many, &plan); err != nil || len(plan) != 1 || plan[0]["type"] != "plan" {
		t.Fatalf("two tasks rendered as %s, want one plan block", many)
	}
	cards, _ := plan[0]["tasks"].([]any)
	if len(cards) != 2 {
		t.Fatalf("the plan block carries %d task(s), want 2: %s", len(cards), many)
	}
	second, _ := cards[1].(map[string]any)
	if second["status"] != "pending" {
		t.Fatalf("an unmapped status rendered as %v, want the fail-closed pending", second["status"])
	}
	first, _ := cards[0].(map[string]any)
	if _, ok := first["details"]; !ok {
		t.Fatalf("a task's detail was dropped instead of rendering as the details rich text: %s", many)
	}
}

// The vendor's table limits are TRUNCATION POINTS, and every cut says so. A silent cut is a wrong answer read
// as a complete one (the ThreadReply/TruncateMarkdown rule, applied to structure).
func TestTableTruncationIsVisible(t *testing.T) {
	columns := make([]string, 30)
	for i := range columns {
		columns[i] = "c"
	}
	rows := make([][]string, 400)
	for i := range rows {
		row := make([]string, 30)
		for j := range row {
			row[j] = "cell"
		}
		rows[i] = row
	}
	blocks := RenderBlocks([]Result{{Type: ResultTable, Columns: columns, Rows: rows}})
	var decoded []map[string]any
	if err := json.Unmarshal(blocks, &decoded); err != nil {
		t.Fatalf("decode blocks: %v", err)
	}
	table := decoded[0]
	if table["type"] != "table" {
		t.Fatalf("first block is %v, want a table", table["type"])
	}
	tableRows, _ := table["rows"].([]any)
	if len(tableRows) > MaxTableRows {
		t.Fatalf("table carries %d rows, want at most %d", len(tableRows), MaxTableRows)
	}
	for i, row := range tableRows {
		cells, _ := row.([]any)
		if len(cells) > MaxTableColumns {
			t.Fatalf("row %d carries %d columns, want at most %d", i, len(cells), MaxTableColumns)
		}
	}
	if !strings.Contains(string(blocks), truncationMarker) {
		t.Fatalf("a table was cut on two axes and said so nowhere: %s", blocks)
	}

	// The third axis is the CHARACTER budget, and it is a separate cut from the row/column caps — a table of
	// four columns and ten rows can still blow 10,000 characters. The budget counts the cells' own text, not
	// the JSON envelope around it.
	huge := make([][]string, 10)
	for i := range huge {
		huge[i] = []string{strings.Repeat("x", 4000)}
	}
	blocks = RenderBlocks([]Result{{Type: ResultTable, Rows: huge}})
	chars := 0
	for _, text := range decodedStrings(t, blocks) {
		if text != "raw_text" && text != "table" && text != "rows" {
			chars += len([]rune(text))
		}
	}
	if chars > MaxTableChars+len([]rune(truncationMarker)) {
		t.Fatalf("the table carries %d characters of cell text; the vendor budget is %d per table", chars, MaxTableChars)
	}
	if !strings.Contains(string(blocks), truncationMarker) {
		t.Fatalf("the character budget cut the table and said so nowhere: %s", blocks)
	}
}

func TestBlockLimitIsVisible(t *testing.T) {
	var results []Result
	for i := 0; i < 80; i++ {
		results = append(results, Result{Type: ResultTable, Rows: [][]string{{"x"}}})
	}
	blocks := RenderBlocks(results)
	var decoded []map[string]any
	if err := json.Unmarshal(blocks, &decoded); err != nil {
		t.Fatalf("decode blocks: %v", err)
	}
	if len(decoded) != MaxBlocks {
		t.Fatalf("rendered %d blocks, want the documented ceiling of %d", len(decoded), MaxBlocks)
	}
	if !strings.Contains(string(blocks), truncationMarker) {
		t.Fatalf("blocks were dropped silently: %s", blocks)
	}
}

// Broadcast neutralisation on the BLOCK path. The text path already routes through NeutralizeBroadcasts
// inside TruncateMarkdown and ThreadReply (E20 T1); block text reaches Slack through neither, so the
// renderer calls the SAME function rather than growing a third copy of the rule.
func TestBlocksNeutralizeBroadcastTokens(t *testing.T) {
	blocks := RenderBlocks([]Result{
		{Type: ResultTable, Columns: []string{"<!channel>"}, Rows: [][]string{{"<@U123>"}}},
		{Type: ResultTasks, Tasks: []Task{{ID: "t1", Title: "ping <!here>", Status: "open", Detail: "<!channel>"}}},
		{Type: ResultFileRef, URL: "https://palai.test/a.txt", Label: "<!channel>"},
	})
	texts := strings.Join(decodedStrings(t, blocks), "\n")
	for _, token := range []string{"<!channel>", "<!here>", "<@U123>"} {
		if strings.Contains(texts, token) {
			t.Fatalf("a rendered block carries a live %s; every broadcast token on this path is defused: %s", token, blocks)
		}
	}
	if !strings.Contains(texts, "&lt;!channel") {
		t.Fatalf("the token was deleted rather than defused; the reader must still see what the model wrote: %s", blocks)
	}
}

// A file_ref is a LINK, and a link is a place a model gets to send a reader. Anything that is not plain http(s)
// falls to inert text rather than becoming a clickable scheme we never reasoned about.
func TestFileRefLinksOnlyHTTPAndHTTPS(t *testing.T) {
	ok := RenderBlocks([]Result{{Type: ResultFileRef, URL: "https://palai.test/artifacts/a.txt", Label: "a.txt"}})
	if !strings.Contains(strings.Join(decodedStrings(t, ok), "\n"), "<https://palai.test/artifacts/a.txt|a.txt>") {
		t.Fatalf("an https artifact link did not render: %s", ok)
	}
	for _, hostile := range []string{
		"javascript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"slack://channel?id=C1",
		"https://palai.test/a|>click me<https://evil.test",
	} {
		blocks := RenderBlocks([]Result{{Type: ResultFileRef, URL: hostile, Label: "download"}})
		// Decoded, because encoding/json escapes the angle brackets a link is made of (see decodedStrings).
		if strings.Contains(strings.Join(decodedStrings(t, blocks), "\n"), "<"+hostile) {
			t.Fatalf("%q rendered as a live link: %s", hostile, blocks)
		}
	}
}

// The unchanged path, pinned: an ordinary prose answer is still exactly the markdown_text chat.stopStream
// carried before this task existed, and it grows NO blocks. A renderer that decorated every answer would be
// a change to every reply in the workspace, not a new capability.
func TestPlainProseIsUnchangedAndGrowsNoBlocks(t *testing.T) {
	const answer = "Shipped. The migration is applied."
	markdown, blocks := RenderOutput(answer, nil, "")
	if markdown != answer {
		t.Fatalf("markdown = %q, want the answer verbatim", markdown)
	}
	if blocks != nil {
		t.Fatalf("prose grew blocks: %s", blocks)
	}
	// Nor does JSON that is not ours: an object with a foreign tag is inert text, not a decoration.
	markdown, blocks = RenderOutput(`{"type":"video","video_url":"https://evil.test/x.mp4"}`, nil, "")
	if blocks != nil {
		t.Fatalf("a foreign JSON tag rendered as %s, want inert text and no blocks", blocks)
	}
	if !strings.Contains(markdown, "video_url") {
		t.Fatalf("the foreign object was dropped rather than shown: %q", markdown)
	}
}

// E21 T6 — M13, THE FIDELITY FIX, and the loss it closes was MEASURED rather than assumed: chat.stopStream
// already carries the model's prose in `markdown_text` (12,000 characters of real markdown), while
// chat.postMessage carries it in `text`, which is Slack's own narrower mrkdwn dialect. So a fenced code block
// loses its language, a header renders as a literal `#`, a table does not render at all and a nested list
// collapses — on the plain-post path ONLY. The markdown block is the vendor's answer to exactly that.
func TestPostMessageRendersProseAsAMarkdownBlock(t *testing.T) {
	const answer = "# Result\n\n```go\nfunc main() {}\n```\n\n| suite | result |\n| --- | --- |\n| slack | pass |"
	var body struct {
		Text   string           `json:"text"`
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(ReplyMessage("C1", "1.1", answer, "resp_1", ""), &body); err != nil {
		t.Fatalf("decode the posted body: %v", err)
	}
	if len(body.Blocks) != 1 || body.Blocks[0]["type"] != "markdown" {
		t.Fatalf("prose rendered as %v, want exactly one markdown block", body.Blocks)
	}
	if body.Blocks[0]["text"] != answer {
		t.Fatalf("the markdown block carries %q, want the model's markdown verbatim", body.Blocks[0]["text"])
	}
	// block_id is "ignored in markdown blocks and will not be retained" — sending one asks Slack to remember
	// something it has said it forgets.
	if _, ok := body.Blocks[0]["block_id"]; ok {
		t.Fatalf("the markdown block carries a block_id the reference says is discarded: %v", body.Blocks[0])
	}
	// text stays the answer. With blocks present Slack renders the blocks and uses text as the NOTIFICATION
	// fallback, so dropping it would silently empty every push notification in the workspace.
	if body.Text != answer {
		t.Fatalf("text = %q, want the answer as the notification fallback", body.Text)
	}
}

// A RICHER RENDER IS NOT A WEAKER DEFENCE (plan §2, non-negotiable). The markdown block's text goes through
// the SAME NeutralizeBroadcasts every other path uses, and the check is over DECODED strings for the reason
// decodedStrings gives: a raw substring assertion over marshalled bytes cannot fail.
func TestMarkdownBlockNeutralizesBroadcastTokens(t *testing.T) {
	var body struct {
		Blocks json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(ReplyMessage("C1", "1.1", "ping <!channel> and <!here> and <@U123>", "", ""), &body); err != nil {
		t.Fatalf("decode the posted body: %v", err)
	}
	texts := strings.Join(decodedStrings(t, body.Blocks), "\n")
	for _, token := range []string{"<!channel>", "<!here>", "<@U123>"} {
		if strings.Contains(texts, token) {
			t.Fatalf("a markdown block carries a live %s; the richer render did not get to skip the defence: %s", token, body.Blocks)
		}
	}
	if !strings.Contains(texts, "&lt;!channel") {
		t.Fatalf("the token was deleted rather than defused; the reader must still see what the model wrote: %s", body.Blocks)
	}
}

// The 12,000 budget is CUMULATIVE ACROSS THE PAYLOAD, not per block — so this fixture is three parts none of
// which is over the limit alone. A per-block check would pass it and ship 15,000 characters.
func TestMarkdownBudgetIsCumulativeAndCutsVisibly(t *testing.T) {
	part := strings.Repeat("ü", 5000) // multi-byte on purpose: the budget counts runes, not bytes
	answer := `[{"type":"text","text":"` + part + `"},{"type":"text","text":"` + part + `"},{"type":"text","text":"` + part + `"}]`
	var body struct {
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(ReplyMessage("C1", "1.1", answer, "", ""), &body); err != nil {
		t.Fatalf("decode the posted body: %v", err)
	}
	chars := 0
	for _, block := range body.Blocks {
		if block["type"] != "markdown" {
			continue
		}
		text, _ := block["text"].(string)
		chars += len([]rune(text))
	}
	if chars == 0 {
		t.Fatalf("no markdown block rendered at all, so the budget assertion below proves nothing: %v", body.Blocks)
	}
	if chars > MaxMarkdownText {
		t.Fatalf("the payload carries %d markdown characters; the vendor's cumulative budget is %d", chars, MaxMarkdownText)
	}
	raw, _ := json.Marshal(body.Blocks)
	if !strings.Contains(strings.Join(decodedStrings(t, raw), "\n"), truncationMarker) {
		t.Fatalf("15,000 characters became %d and said so nowhere: %s", chars, raw)
	}
}

// M20(d), FAIL-CLOSED. No published page says chat.stopStream's `blocks` array accepts a markdown block, and
// a vendor's silence is not a design freedom (E20 S12's rule). So the streaming path keeps the section it has
// always sent, and this test is what stops a later reader from "finishing the job" without a live measurement.
// The visible cost is named in the plan: a run that ends by streaming gets the older render.
func TestTheStopStreamPathCarriesNoMarkdownBlock(t *testing.T) {
	_, blocks := RenderOutput(forgedOutput, []Task{{ID: "t1", Title: "Write it", Status: "done"}}, "")
	var decoded []map[string]any
	if err := json.Unmarshal(blocks, &decoded); err != nil {
		t.Fatalf("decode blocks: %v (%s)", err, blocks)
	}
	for i, block := range decoded {
		if block["type"] == "markdown" {
			t.Fatalf("block %d on the chat.stopStream path is a markdown block; that it is accepted there is UNCONFIRMED (M20(d)) and must be measured live before it ships: %s", i, blocks)
		}
	}
	// And the direct render still yields a section, so the claim is about the SURFACE rather than about
	// RenderOutput happening to route prose into markdown_text.
	one := RenderBlocks([]Result{{Type: ResultText, Text: "hi"}})
	if !strings.Contains(string(one), `"type":"section"`) {
		t.Fatalf("the stopStream render of a text result is %s, want the section it has always been", one)
	}
	// THE ASSERTION ABOVE MUST DISCRIMINATE. The SAME fixture on the postMessage surface does carry a
	// markdown block — without this the test would keep passing after the block was deleted entirely, and
	// would be certifying nothing.
	if !strings.Contains(string(ReplyMessage("C1", "1.1", forgedOutput, "", "")), `"type":"markdown"`) {
		t.Fatal("the postMessage surface carries NO markdown block either, so the stopStream assertion above proves nothing")
	}
}

// M14 — the table's one cheap enrichment. A cell that IS a number is typed as one, which is what makes Slack
// right-align it (and sort the column numerically). Everything else stays raw_text, the type with the least
// interpretation.
func TestNumericTableCellsAreRawNumber(t *testing.T) {
	blocks := RenderBlocks([]Result{{Type: ResultTable,
		Columns: []string{"suite", "failures"},
		Rows:    [][]string{{"slack", "0"}, {"render", "12"}, {"total", "1.5s"}},
	}})
	var decoded []struct {
		Rows [][]map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(blocks, &decoded); err != nil {
		t.Fatalf("decode blocks: %v (%s)", err, blocks)
	}
	rows := decoded[0].Rows
	if len(rows) != 4 {
		t.Fatalf("rendered %d rows, want the header plus three: %s", len(rows), blocks)
	}
	for _, cell := range []map[string]any{rows[0][0], rows[0][1], rows[1][0], rows[3][1]} {
		if cell["type"] != "raw_text" {
			t.Fatalf("cell %v is typed %v; a cell that is not a number stays raw_text", cell["text"], cell["type"])
		}
	}
	for _, cell := range []map[string]any{rows[1][1], rows[2][1]} {
		if cell["type"] != "raw_number" {
			t.Fatalf("numeric cell %v is typed %v, want raw_number", cell["text"], cell["type"])
		}
		// NO `value` KEY, and this is a guard rather than a shape preference: a button's `value` is the
		// payload it dispatches, so `value` is one of the two field names the actionable sweep hunts for
		// (SweepActionableElements, sweepActionable). A numeric cell carrying one would count as a forged
		// interaction in every evidence bundle, and the only way to green that again is to weaken the sweep.
		if _, ok := cell["value"]; ok {
			t.Fatalf("a raw_number cell carries a `value` key, which the actionable sweep reads as a dispatchable element: %v", cell)
		}
		if cell["text"] == "" {
			t.Fatalf("a raw_number cell lost the text Slack draws: %v", cell)
		}
	}
	if fmt.Sprint(rows[2][1]["text"]) != "12" {
		t.Fatalf("the raw_number cell draws %v, want the cell's own digits", rows[2][1]["text"])
	}
	// The sweep must stay CLEAN over a numeric table — the collision above, asserted where it would bite.
	if found := sweepJSON(t, "numeric table", blocks); len(found) != 0 {
		t.Fatalf("a numeric table registered %d actionable element(s): %v", len(found), found)
	}
}

// The journal's tasks reach the answer even when the model wrote nothing but prose — this is the wiring that
// gives the task card a user today (the follower collects task.created/updated.v1, the reply pump renders it
// when it closes the stream).
func TestJournalTasksRenderBesideProse(t *testing.T) {
	markdown, blocks := RenderOutput("Done.", []Task{{ID: "t1", Title: "Write the migration", Status: "done"}}, "")
	if markdown != "Done." {
		t.Fatalf("markdown = %q, want the prose answer", markdown)
	}
	if !strings.Contains(string(blocks), `"task_card"`) || !strings.Contains(string(blocks), "Write the migration") {
		t.Fatalf("the journal's tasks did not render: %s", blocks)
	}
	if found := sweepJSON(t, "journal tasks", blocks); len(found) != 0 {
		t.Fatalf("task cards carried %d actionable element(s): %v", len(found), found)
	}
}

// X14b: a task card carries the places a reader can go and CHECK — for E22 those are the pull request the run
// opened and the Jira ticket it worked from. E21 T6 left the field out because nothing filled it; this is the
// render half, and the trust boundary is the same one every link in this package crosses.
func TestTaskCardCarriesItsSourcesAndValidatesEveryURL(t *testing.T) {
	blocks := RenderBlocks([]Result{{Type: ResultTasks, Tasks: []Task{{
		ID: "t1", Title: "Ship the screen recorder", Status: "done",
		Sources: []TaskSource{
			{URL: "https://github.com/owner/repo/pull/7", Text: "the pull request"},
			{URL: "https://example.atlassian.net/browse/PAL-42"},                  // no text: the host labels it
			{URL: "javascript:alert(1)", Text: "click me"},                        // not a link
			{URL: "https://evil.test/x|<@U1>", Text: "breaks out of <url|label>"}, // syntax break-out
			{URL: "ftp://files.test/x", Text: "wrong scheme"},                     // not http(s)
		},
	}}}})
	var decoded []map[string]any
	if err := json.Unmarshal(blocks, &decoded); err != nil {
		t.Fatalf("decode blocks: %v (%s)", err, blocks)
	}
	if len(decoded) != 1 || decoded[0]["type"] != "task_card" {
		t.Fatalf("one task rendered as %s, want a bare task_card", blocks)
	}
	sources, _ := decoded[0]["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("the card carried %d source(s), want the 2 that are real links: %s", len(sources), blocks)
	}
	first, _ := sources[0].(map[string]any)
	if first["type"] != "url" || first["url"] != "https://github.com/owner/repo/pull/7" || first["text"] != "the pull request" {
		t.Fatalf("source 0 = %v, want the documented {type,url,text} element", first)
	}
	second, _ := sources[1].(map[string]any)
	if second["text"] != "example.atlassian.net" {
		t.Fatalf("a source with no text was labelled %q, want its host", second["text"])
	}
	// A REFUSED source is dropped, not rendered — including its text, which is a place a model writes words.
	for _, gone := range []string{"javascript:", "click me", "evil.test", "ftp://"} {
		if strings.Contains(string(blocks), gone) {
			t.Fatalf("a refused source left %q in the card: %s", gone, blocks)
		}
	}

	// The cap is ours and it holds.
	many := make([]TaskSource, MaxTaskSources+5)
	for i := range many {
		many[i] = TaskSource{URL: "https://example.test/" + string(rune('a'+i))}
	}
	capped := RenderBlocks([]Result{{Type: ResultTasks, Tasks: []Task{{ID: "t2", Title: "many", Sources: many}}}})
	if err := json.Unmarshal(capped, &decoded); err != nil {
		t.Fatalf("decode capped blocks: %v", err)
	}
	if got, _ := decoded[0]["sources"].([]any); len(got) != MaxTaskSources {
		t.Fatalf("%d sources survived the cap, want %d", len(got), MaxTaskSources)
	}
}

// A source's TEXT is a string a model wrote, so it is defused exactly like every other one — and the check
// JSON-DECODES first, because encoding/json escapes `<` and a raw-substring assertion over marshalled bytes
// could never fail (E20 T4).
func TestTaskCardSourceTextIsNeutralised(t *testing.T) {
	blocks := RenderBlocks([]Result{{Type: ResultTasks, Tasks: []Task{{
		ID: "t1", Title: "x", Sources: []TaskSource{{URL: "https://example.test/pr/1", Text: "hey <!channel> look"}},
	}}}})
	for _, s := range decodedStrings(t, blocks) {
		if strings.Contains(s, "<!channel>") {
			t.Fatalf("a live broadcast token survived in a source's text: %q", s)
		}
	}
	found := false
	for _, s := range decodedStrings(t, blocks) {
		if strings.Contains(s, "channel") {
			found = true
		}
	}
	if !found {
		t.Fatal("the source text vanished entirely rather than being defused, so the assertion above proved nothing")
	}
}
