package stack

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestBringUpKnowsNothingAboutSlack is the boundary this epic exists to draw: `palai up` brings a stack
// up and PROVES it live, and a chat integration is not part of that. Slack is a bot registered through
// POST /v1/bots and run by apps/slack-bot, which reads its own row — so a bring-up that also stored Slack
// credentials, registered a workspace, provisioned a "slack" agent profile and watched a Socket Mode log
// was a second, undocumented configuration path for a product the panel already owns.
//
// It reads BYTES rather than the AST on purpose. A symbol sweep sees `wireSlack` coming back and misses
// the shapes this file actually loses control to: an env var named in a comment, a warning that mentions
// SLACK_APPROVER_IDS, a log needle. The whole word is the guard, case-insensitively, and the count is the
// only tolerance there is.
func TestBringUpKnowsNothingAboutSlack(t *testing.T) {
	src, err := os.ReadFile("up.go")
	if err != nil {
		t.Fatal(err)
	}
	var where []string
	for i, line := range strings.Split(string(src), "\n") {
		if bytes.Contains(bytes.ToLower([]byte(line)), []byte("slack")) {
			where = append(where, fmt.Sprintf("up.go:%d: %s", i+1, strings.TrimSpace(line)))
		}
	}
	if len(where) > 0 {
		if len(where) > 10 {
			where = append(where[:10], fmt.Sprintf("... and %d more lines", len(where)-10))
		}
		t.Fatalf("up.go mentions slack on %d line(s); bring-up prepares a stack and nothing else:\n%s",
			len(where), strings.Join(where, "\n"))
	}
}
