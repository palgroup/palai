package toolbroker

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ArgumentSummaryLimit bounds an argument summary, in bytes, INCLUDING the truncation marker.
//
// MEASURED, not chosen (2026-08-04, the operator's own live ledger, 123 tool calls):
//
//	max(octet_length(arguments::text))                       → 216 bytes
//	max(length(rendered summary))                            → 173 chars
//	avg(length(rendered summary))                            →  54 chars
//	count(*) where length(rendered summary) > 200            →   0
//
// So 256 truncates nothing this deployment has ever run, and that is the point: the ceiling exists to
// bound the pathological call, not to trim the ordinary one. Against the 51,422 bytes a trivial
// `xcodebuild` RESULT measures on this machine — the number that kept arguments off this frame in the
// first place — it is a ~200× smaller commitment per event.
const ArgumentSummaryLimit = 256

// argumentSummaryEllipsis marks a truncated summary. Truncation is never silent: a reader that sees a
// summary end here knows the call had more to it and the ledger read has the rest.
const argumentSummaryEllipsis = "…"

// argumentSummarisers renders one line per tool from that tool's OWN argument shape.
//
// IT IS AN ALLOW-LIST AND IT FAILS CLOSED. A tool absent from this map produces no summary at all, and
// the frame carries only the name it already carried. That is deliberate and it is the sensitivity
// answer: the summarised tools are the ones whose input schema THIS deployment authored, so their
// argument shape is known here and cannot change under us. An MCP or remote-HTTP tool's schema is
// written by a third party — it may declare an `api_key` property tomorrow that the model dutifully
// fills — so those tools get nothing until somebody writes them a summariser and reads their schema
// while doing it. A blocklist of secret-looking key names would have been the other option and it
// fails OPEN, which this tree has paid for before.
//
// EVERY SUMMARISER SELECTS IDENTIFYING FIELDS AND NEVER CONTENT FIELDS. `file` renders op+path and not
// `content`; the editor renders command+path and not `file_text`/`old_str`/`new_str`; the pull request
// renders its title and not its body. Those excluded fields are whole-file and free-prose bytes — the
// same unbounded, model-authored class as a tool RESULT, which is exactly what must not fan out to
// every registered webhook endpoint and be stored per delivery. Keeping them out by CONSTRUCTION is
// why the ceiling above is a backstop rather than the mechanism.
//
// Only tools that can actually reach the emitter are here. `tool_call.executing.v1` is journalled by
// coordinator.BeginToolCall, which runs when NeedsPreWrite(class) — Irreversible, Reversible,
// Interactive — or when a human approved the call. That is shell, file, the text editor and commit by
// class, plus the gated publication tools by approval. Measured against the live journal on 2026-08-04,
// `palai.workspace.show_media` has 2 ledger rows and 0 executing frames, exactly as its Pure class
// predicts; a summariser for it would be code that cannot run.
var argumentSummarisers = map[string]func(map[string]any) string{
	"palai.workspace.shell": func(a map[string]any) string {
		items, _ := a["argv"].([]any)
		parts := make([]string, 0, len(items))
		for _, it := range items {
			if s, ok := it.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	},
	"palai.workspace.file": func(a map[string]any) string {
		return joinFields(stringField(a, "op"), stringField(a, "path"))
	},
	"str_replace_based_edit_tool": func(a map[string]any) string {
		return joinFields(stringField(a, "command"), stringField(a, "path"))
	},
	"palai.workspace.commit": func(a map[string]any) string {
		// The first line only: a commit message is a subject line and then prose, and the subject is the
		// part that identifies the commit.
		msg := stringField(a, "message")
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return msg
	},
	"palai.publish.pull_request": func(a map[string]any) string {
		return stringField(a, "title")
	},
	// palai.publish.push and palai.publish.merge_pull_request are deliberately absent rather than mapped
	// to an empty renderer: both declare `"properties": {}` with additionalProperties false, so they
	// carry no arguments to summarise — their destination is resolved from the run's binding and is
	// never model-supplied. For those two the tool name already is the whole of what happened.
}

// ArgumentSummary renders a short, redacted, bounded description of what a tool call is about to do,
// for the event frame that until now said only THAT a tool ran.
//
// WHY A SUMMARY RATHER THAN THE RAW ARGUMENTS. The raw `arguments` JSON has no ceiling — an MCP tool's
// schema is a third party's to write — and an event payload is not a private thing: webhook_pump.go
// puts it in the body it POSTs to every registered endpoint and stores that envelope immutably for
// byte-for-byte redelivery, and the audit chain hashes it. A summary is bounded by construction, and it
// puts the per-tool knowledge of WHICH field identifies a call in one place instead of in every
// consumer — the Slack card, the console and any webhook receiver would otherwise each need to learn
// that `shell` means argv and `file` means path. The cost is that it is LOSSY on purpose: a caller that
// needs the exact arguments still reads GET /v1/responses/{id}/tool-calls, which is unchanged. What
// this removes is the round trip for the LABEL, which was the complaint.
//
// REDACTION IS SHAPE-BASED HERE AND THAT CEILING IS REAL. RedactSecrets runs; RedactValues does not,
// because it needs the attempt's resolved environment and that map does not exist yet at this point in
// dispatch — resolveEnvValues is called IMMEDIATELY before Execute on purpose, so that a run parked
// waiting for a human to approve a tool holds no credential in memory. Pulling it earlier to mask a
// caption would trade a security property for a label, so the shape redactor runs alone and this
// sentence says so. In practice the model cannot easily hold an environment VALUE to type into an argv,
// because every result it has ever seen went through RedactValues on the way back. Note also what this
// still improves on: `tool_calls.arguments` is journalled with NO redaction at all today, so the
// summary is the better-masked of the two copies.
//
// Redaction runs BEFORE truncation, and that order is load-bearing: cutting first can leave `sk-ABC`,
// a prefix too short for the `{6,}` quantifier to match, and ship a partial credential the redactor
// would have caught at full length.
func ArgumentSummary(name string, arguments []byte) string {
	render, ok := argumentSummarisers[name]
	if !ok || len(arguments) == 0 {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal(arguments, &args); err != nil {
		return ""
	}
	summary := strings.TrimSpace(render(args))
	if summary == "" {
		return ""
	}
	return truncateSummary(RedactSecrets(summary))
}

// truncateSummary bounds s to ArgumentSummaryLimit BYTES — bytes, because the thing being bounded is
// what an event costs on the wire and in every stored delivery — while cutting only on a rune boundary
// so a multi-byte character is never split into mojibake.
func truncateSummary(s string) string {
	if len(s) <= ArgumentSummaryLimit {
		return s
	}
	cut := ArgumentSummaryLimit - len(argumentSummaryEllipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + argumentSummaryEllipsis
}

// stringField reads a string property, tolerating absence and a wrong type — a summary is a caption and
// must never be the reason a tool call fails.
func stringField(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// joinFields joins the non-empty parts with a space, so a call missing one of them reads as the other
// rather than as a stray separator.
func joinFields(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}
