package slack

import (
	"encoding/json"
	"strings"
	"testing"
)

// THE MENTION (E21 T3, plan §2). E20 T1 built a defence and this file guards its EXTENSION, so the sentence
// being tested is worth stating exactly: the rule is not "the model may not address a human", it is "text the
// MODEL WROTE may not address a human". The model declares a typed intent; the `<@U…>` token is minted here,
// from an id the model never touches — structurally the same arrangement that makes interactions.go the sole
// mint of a button.
//
// Every assertion below runs over `outbound`, never over RenderOutput's raw return, for the reason
// decodedStrings gives about the block path: the markdown half is neutralized ONE LAYER LATER (StopStream ->
// TruncateMarkdown), so an assertion on the raw string would pass while the wire carried a live token.

// requesterID is the one identity a rendered answer can reach: the person whose message birthed the run,
// frozen on the delivery row at enqueue time.
const requesterID = "U0REQUESTER"

// outbound is what chat.stopStream actually sends for one rendered answer — the markdown_text after the text
// path's own defence, plus every string inside the blocks.
func outbound(t *testing.T, markdown string, blocks json.RawMessage) string {
	t.Helper()
	parts := []string{TruncateMarkdown(markdown)}
	if len(blocks) > 0 {
		parts = append(parts, decodedStrings(t, blocks)...)
	}
	return strings.Join(parts, "\n")
}

// RED #1 (plan §4 T3): a `<@U…>` the MODEL wrote is still defused, in prose and inside the mention variant's
// own text. NeutralizeBroadcasts is untouchable; the mention adds a token AFTER it rather than opening a hole
// through it.
func TestAMentionTheModelWroteIsStillNeutralized(t *testing.T) {
	markdown, blocks := RenderOutput("ping <@U012AB3CD> about the branch", nil, requesterID)
	prose := outbound(t, markdown, blocks)
	if strings.Contains(prose, "<@U012AB3CD>") || !strings.Contains(prose, "&lt;@U012AB3CD>") {
		t.Fatalf("a raw mention in PROSE reached the wire live: %q", prose)
	}

	// The same token inside the typed variant, where the mint happens: the model's copy is escaped and ours
	// is not, in one rendered answer.
	markdown, blocks = RenderOutput(
		`{"type":"mention","who":"requester","text":"tell <@U012AB3CD> which branch"}`, nil, requesterID)
	typed := outbound(t, markdown, blocks)
	if strings.Contains(typed, "<@U012AB3CD>") || !strings.Contains(typed, "&lt;@U012AB3CD>") {
		t.Fatalf("a raw mention inside the mention variant reached the wire live: %q", typed)
	}
	if strings.Count(typed, "<@") != 1 || !strings.Contains(typed, "<@"+requesterID+">") {
		t.Fatalf("want exactly one live mention and it OURS, got %q", typed)
	}
}

// RED #2: the typed intent mints EXACTLY ONE token, and it is the delivery row's id — not a second one, not a
// broadcast, and not an id that appears anywhere in what the model wrote.
func TestMentionMintsExactlyOneIDAndItIsTheDeliveryRows(t *testing.T) {
	markdown, blocks := RenderOutput(
		`{"type":"mention","who":"requester","text":"which branch should I publish?"}`, nil, requesterID)
	got := outbound(t, markdown, blocks)
	if strings.Count(got, "<@") != 1 {
		t.Fatalf("minted %d mention tokens, want exactly one: %q", strings.Count(got, "<@"), got)
	}
	if !strings.Contains(got, "<@"+requesterID+"> which branch should I publish?") {
		t.Fatalf("the minted token is not the delivery row's id in front of the model's words: %q", got)
	}
	if strings.Contains(got, "<!") {
		t.Fatalf("a mention became a BROADCAST: %q", got)
	}
}

// RED #3: the model CANNOT CHOOSE AN IDENTITY. `who` is a closed enum whose only value is `requester`; a
// fixture that writes a user id into it mints NOTHING and falls to inert text like any other variant we did
// not choose. This is the test plan §2 calls the condition of ever adding a second identity.
func TestTheModelCannotChooseWhoIsMentioned(t *testing.T) {
	for _, who := range []string{"U012AB3CD", "<@U012AB3CD>", "channel", "here", ""} {
		answer, err := json.Marshal(map[string]string{"type": "mention", "who": who, "text": "hello"})
		if err != nil {
			t.Fatalf("build the fixture: %v", err)
		}
		markdown, blocks := RenderOutput(string(answer), nil, requesterID)
		got := outbound(t, markdown, blocks)
		if strings.Contains(got, "<@") || strings.Contains(got, "<!") {
			t.Fatalf("who=%q minted a live token: %q", who, got)
		}
		if !strings.Contains(got, "hello") {
			t.Fatalf("who=%q was silently dropped rather than falling to inert text: %q", who, got)
		}
	}
}

// RED #4 (fail-closed): an EMPTY requester — every row written before migration 000043, which carries
// DEFAULT ” — skips the mention and sends the words without one. No invented id, and no fallback to
// `<!here>`: a notification we cannot address correctly is one we do not send.
func TestAnEmptyRequesterSkipsTheMentionAndKeepsTheWords(t *testing.T) {
	markdown, blocks := RenderOutput(
		`{"type":"mention","who":"requester","text":"which branch should I publish?"}`, nil, "")
	got := outbound(t, markdown, blocks)
	if strings.Contains(got, "<@") || strings.Contains(got, "<!") {
		t.Fatalf("an empty requester still produced a token: %q", got)
	}
	if !strings.Contains(got, "which branch should I publish?") {
		t.Fatalf("the answer was lost with the mention: %q", got)
	}
}

// RED #5 (§3.6 D6): chat.update was OUTSIDE the broadcast defence, and what it interpolates is a publication
// display built from the RUN'S OWN BRANCH NAME — and `<!channel>` is a valid git ref. The model-influenced
// half is neutralized; the decider's ping, which this file mints from the id on the verified click, survives.
func TestAPublicationDisplayCannotBroadcast(t *testing.T) {
	body := UpdateMessage("C1", "1700000000.000100", "approved: publish <!channel> to main", "U0DECIDER")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode the update body: %v", err)
	}
	text, _ := got["text"].(string)
	if strings.Contains(text, "<!channel>") || !strings.Contains(text, "&lt;!channel>") {
		t.Fatalf("a branch named <!channel> reached chat.update live: %q", text)
	}
	if strings.Count(text, "<@") != 1 || !strings.Contains(text, "<@U0DECIDER>") {
		t.Fatalf("want exactly the decider's ping minted here, got %q", text)
	}
	// The blocks carry the same repaired text, so the sweep has to hold on both fields.
	for _, s := range decodedStrings(t, body) {
		if strings.Contains(s, "<!channel>") {
			t.Fatalf("the update's blocks carried a live broadcast: %q", s)
		}
	}
}


// The mention is a TEXT TOKEN, not an actionable element, so it must not need an exception anywhere: the
// singularity sweep in blocks_test.go stays green with the renderer minting one. Asserted here as well
// because "it renders as a section like any other text" is the property that keeps it out of that test's
// scope — if a mention ever became its own block type, this fails before the sweep has to.
func TestAMentionRendersAsAnOrdinarySection(t *testing.T) {
	_, blocks := RenderOutput(`{"type":"mention","who":"requester","text":"which branch?"}`, nil, requesterID)
	var decoded []map[string]any
	if err := json.Unmarshal(blocks, &decoded); err != nil {
		t.Fatalf("decode blocks: %v (%s)", err, blocks)
	}
	if len(decoded) != 1 || decoded[0]["type"] != "section" {
		t.Fatalf("a mention rendered as %s, want one ordinary section", blocks)
	}
	if found := sweepJSON(t, "mention", blocks); len(found) != 0 {
		t.Fatalf("a mention minted %d actionable element(s): %v", len(found), found)
	}
}
