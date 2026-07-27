package extensions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// E20 T3's FOURTH authority boundary, and the heaviest: an app_context entity may never become a FETCH
// TARGET.
//
// WHY IT IS THE HEAVY ONE. The app holds `channels:history`. The context reports what the HUMAN is looking
// at. The run executes with the CONNECTION PRINCIPAL's authority. Those are different parties, so a code
// path that read a channel because a context named it would be a confused-deputy read primitive: the user's
// VIEW would grant them the connection's ACCESS, and a private channel the connection can see would be
// readable by anyone who can open it on their own screen and then DM the bot.
//
// WHY A BEHAVIOURAL TEST IS NOT ENOUGH. The component leg (store/slack_context_component_test.go) proves
// today's admission fetches nothing. It cannot prove tomorrow's does not — a resolver added next to it would
// leave that test green, because that test only drives the paths it knows about. This one is the structural
// half: the METHOD NAMES that could turn an id into content may not appear as string literals anywhere in
// the Slack seam, so a resolver cannot appear by accident. Adding one becomes a deliberate act that deletes
// a line from this list, in a diff a reviewer reads.
//
// STRING LITERALS, VIA THE PARSER, NOT grep. Two production comments in this very package discuss
// conversations.replies and conversations.info by name — correctly, they are what the code refuses to do. A
// substring scan would be red on day one and would make it impossible to WRITE about the boundary. The AST
// walk sees only what could actually be sent on a wire.
//
// CEILING, named rather than implied: this catches a method name spelled as a literal. A path assembled from
// fragments ("conversations" + "." + "history") or read from configuration would slip past it. That is not
// the failure mode this defends against — an accident is spelled out; a deliberate obfuscation is not an
// accident, and the component leg's zero-call assertion still covers the shipped paths.
func TestSlackNoCodePathResolvesAContextEntity(t *testing.T) {
	// The Slack seam: the wire adapter and the control-plane bridge. A fetch would have to be written in one
	// of them — every other package reaches Slack through these.
	dirs := []string{".", filepath.Join("..", "..", "..", "..", "adapters", "integrations", "slack")}

	// The Web API methods that turn an id from a context into CONTENT. Each is a read this app must never
	// perform on the strength of what a user happens to be looking at.
	//
	// CONTRACT: https://docs.slack.dev/reference/methods/conversations.history/,
	// .../conversations.replies/, .../conversations.info/, .../conversations.members/,
	// .../conversations.list/, .../search.messages/ (all checked 2026-07-27) — each takes a conversation id
	// (or a query) and answers with its contents or its membership.
	forbidden := []string{
		"conversations.history",
		"conversations.replies",
		"conversations.info",
		"conversations.members",
		"conversations.list",
		"search.messages",
		"canvases.sections.lookup",
		"files.info",
	}

	scanned := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			// Only the Slack seam of this package: extensions also hosts MCP, A2A and hooks, and none of them
			// is this boundary's business.
			if dir == "." && !strings.HasPrefix(name, "slack") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			scanned++
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					value = lit.Value
				}
				for _, method := range forbidden {
					if strings.Contains(value, method) {
						t.Errorf("%s names the Slack Web API method %q in a string literal. A context entity "+
							"describes what a USER is looking at, while the run carries the CONNECTION "+
							"principal's authority — reading a conversation because a context named it is a "+
							"confused-deputy read primitive (E20 T3). If this call is genuinely wanted, it "+
							"earns its own authority decision and its own review, not a line that slipped in.",
							path, method)
					}
				}
				return true
			})
		}
	}
	// A guard that scanned nothing is a guard that proves nothing — the exact green-by-skip this repo has
	// shipped eleven times.
	if scanned < 10 {
		t.Fatalf("the guard parsed only %d Slack source files; it must be scanning the whole seam", scanned)
	}
}
