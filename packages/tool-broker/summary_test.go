package toolbroker

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestArgumentSummaryRendersEachKnownTool pins the rendering per tool, because the caption's whole value
// is that a reader can tell one call from the next — a summary that collapsed two different commands to
// the same line would be the static label this replaced.
func TestArgumentSummaryRendersEachKnownTool(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		args string
		want string
	}{
		{"shell joins argv", "palai.workspace.shell",
			`{"argv":["grep","-rln","Date of birth","repo"],"shell":false}`,
			"grep -rln Date of birth repo"},
		{"file renders op and path", "palai.workspace.file",
			`{"op":"read","path":"repo/Module/NameBirthdayInputScreen.swift"}`,
			"read repo/Module/NameBirthdayInputScreen.swift"},
		{"editor renders command and path", "str_replace_based_edit_tool",
			`{"command":"str_replace","path":"repo/App.swift","old_str":"a","new_str":"b"}`,
			"str_replace repo/App.swift"},
		{"commit renders the subject line", "palai.workspace.commit",
			`{"message":"fix(ui): the birthday field\n\nA longer body that is not the subject."}`,
			"fix(ui): the birthday field"},
		{"pull request renders its title", "palai.publish.pull_request",
			`{"title":"Add the birthday input","body":"a long description"}`,
			"Add the birthday input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArgumentSummary(tc.tool, []byte(tc.args)); got != tc.want {
				t.Fatalf("ArgumentSummary(%s) = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

// TestArgumentSummaryExcludesContentFields is the SIZE property stated as a shape property, and it is why
// the 256-byte ceiling is a backstop rather than the mechanism. `content`, `file_text`, `old_str`,
// `new_str` and a PR `body` are whole-file and free-prose bytes — the same unbounded, model-authored class
// as a tool RESULT, which is precisely what must not fan out to every registered webhook endpoint.
//
// It asserts the ABSENCE OF THE BYTES rather than the shape of the output, so a renderer that started
// appending content in some other format cannot slip past a string-equality check.
func TestArgumentSummaryExcludesContentFields(t *testing.T) {
	needle := "CONTENT-BYTES-THAT-MUST-NOT-RIDE-THE-FRAME"
	for _, tc := range []struct{ tool, args string }{
		{"palai.workspace.file", `{"op":"write","path":"a.swift","content":"` + needle + `"}`},
		{"str_replace_based_edit_tool", `{"command":"create","path":"a.swift","file_text":"` + needle + `"}`},
		{"str_replace_based_edit_tool", `{"command":"str_replace","path":"a.swift","old_str":"` + needle + `","new_str":"x"}`},
		{"palai.publish.pull_request", `{"title":"t","body":"` + needle + `"}`},
		{"palai.workspace.commit", `{"message":"subject\n\n` + needle + `"}`},
	} {
		if got := ArgumentSummary(tc.tool, []byte(tc.args)); strings.Contains(got, needle) {
			t.Fatalf("ArgumentSummary(%s) leaked a content field: %q", tc.tool, got)
		}
	}
}

// TestArgumentSummaryIsUnknownToolClosed is the sensitivity guard. A tool this deployment did not author
// gets NO summary, because its argument schema belongs to whoever wrote the MCP server: it may declare an
// `api_key` property tomorrow and the model will fill it in.
//
// The alternative — summarise everything and mask key names that look secret — is a blocklist, and a
// blocklist fails open on the first name nobody thought of.
func TestArgumentSummaryIsUnknownToolClosed(t *testing.T) {
	for _, tool := range []string{
		"acme.jira.create_ticket",          // an MCP server's tool
		"remote.deploy",                    // a remote-HTTP tool
		"palai.publish.push",               // declares no properties at all
		"palai.publish.merge_pull_request", // likewise
		"palai.workspace.shell.v2",         // a near-miss on a name that IS summarised
	} {
		if got := ArgumentSummary(tool, []byte(`{"api_key":"sk-live-not-a-real-key-000000","argv":["x"]}`)); got != "" {
			t.Fatalf("ArgumentSummary(%s) = %q, want \"\" — an unrecognised argument shape must contribute "+
				"nothing to a payload that fans out to every registered webhook endpoint", tool, got)
		}
	}
}

// TestArgumentSummaryRedactsSecretShapes covers the argv a model composes itself. The ledger's own copy of
// these arguments is stored with NO redaction, so the frame is deliberately the better-masked of the two.
//
// The ceiling is shape-based and stated where the function lives: RedactValues cannot run here because the
// attempt's environment is resolved immediately before Execute, on purpose.
func TestArgumentSummaryRedactsSecretShapes(t *testing.T) {
	for _, secret := range []string{
		"sk-live-abcdef0123456789",
		"ghp_abcdefghijklmnopqrstuvwxyz0123",
		"github_pat_abcdefghijklmnopqrstuvwxyz",
	} {
		args := `{"argv":["curl","-H","Authorization: ` + secret + `","https://api.example.com"]}`
		got := ArgumentSummary("palai.workspace.shell", []byte(args))
		if strings.Contains(got, secret) {
			t.Fatalf("ArgumentSummary leaked a secret-shaped token: %q", got)
		}
		if !strings.Contains(got, "***") {
			t.Fatalf("ArgumentSummary(%q) did not mask anything: %q", secret, got)
		}
	}
}

// TestArgumentSummaryRedactsBeforeItTruncates is the ordering guard, and it is the reason the two steps
// are not interchangeable: truncating first can leave `sk-abc`, a prefix too short for the `{6,}`
// quantifier to match, and ship a partial credential the redactor would have caught at full length.
// THE OFFSET IS THE TEST. A first version of this padded to ArgumentSummaryLimit-10 and stayed GREEN
// under the reversed order, because the cut left TWELVE characters of the secret — `sk-live-abcd`, which
// `sk-[A-Za-z0-9._-]{6,}` still matches, so the late redactor caught it anyway and the ordering made no
// difference. The hazard is the surviving prefix that is too SHORT to match: `keep` is chosen so the cut
// leaves `sk-live`, four characters past `sk-` where the quantifier wants six.
func TestArgumentSummaryRedactsBeforeItTruncates(t *testing.T) {
	const keep = 7 // "sk-live" — under the {6,} threshold, so a late redactor cannot see it
	pad := ArgumentSummaryLimit - len(argumentSummaryEllipsis) - len("echo ") - 1 - keep
	args := `{"argv":["echo","` + strings.Repeat("a", pad) + `","sk-live-abcdef0123456789"]}`

	got := ArgumentSummary("palai.workspace.shell", []byte(args))
	if strings.Contains(got, "sk-live") {
		t.Fatalf("a secret straddling the truncation boundary survived as an unmatchable prefix: %q", got)
	}
	// Non-vacuity: the fixture must actually REACH the boundary, or this proves nothing. Redact-first
	// collapses the secret to *** and the line lands under the ceiling untruncated.
	if !strings.Contains(got, "***") {
		t.Fatalf("the fixture never reached the redactor at all: %q", got)
	}
}

// TestArgumentSummaryIsBounded pins the ceiling and the marker. The bound is in BYTES because what is
// being bounded is the cost of an event on the wire and in every stored delivery.
func TestArgumentSummaryIsBounded(t *testing.T) {
	args := `{"argv":["grep","` + strings.Repeat("A", 8192) + `"]}`
	got := ArgumentSummary("palai.workspace.shell", []byte(args))
	if len(got) > ArgumentSummaryLimit {
		t.Fatalf("ArgumentSummary returned %d bytes, want <= %d", len(got), ArgumentSummaryLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncation must not be silent; got %q", got)
	}
}

// TestArgumentSummaryTruncatesOnARuneBoundary keeps a multi-byte character from being cut into mojibake —
// the caption is rendered to a human in Slack, and half a rune renders as a replacement glyph.
func TestArgumentSummaryTruncatesOnARuneBoundary(t *testing.T) {
	// Turkish text, so the cut lands mid-rune for at least some offsets.
	args := `{"argv":["echo","` + strings.Repeat("ış", 4096) + `"]}`
	got := ArgumentSummary("palai.workspace.shell", []byte(args))
	if len(got) > ArgumentSummaryLimit {
		t.Fatalf("ArgumentSummary returned %d bytes, want <= %d", len(got), ArgumentSummaryLimit)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("ArgumentSummary split a multi-byte rune: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("ArgumentSummary returned invalid UTF-8: %q", got)
	}
}

// TestArgumentSummaryToleratesMalformedArguments — a caption must never be the reason a tool call fails.
func TestArgumentSummaryToleratesMalformedArguments(t *testing.T) {
	for _, args := range []string{``, `not json`, `{"argv":"not-an-array"}`, `{"argv":[1,2,3]}`, `{}`, `null`} {
		if got := ArgumentSummary("palai.workspace.shell", []byte(args)); got != "" {
			t.Fatalf("ArgumentSummary(%q) = %q, want \"\"", args, got)
		}
	}
}
