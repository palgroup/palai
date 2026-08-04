//go:build live

// THE MODAL, PROVED AGAINST THE API THAT REFUSES IT.
//
// WHY THIS LEG EXISTS AND WHY IT IS SHAPED THIS WAY. slack.ToolApprovalModal was written in E23 and never
// driven: the epic recorded "these blocks are validated SYNTACTICALLY against the published references,
// never visually", and the thing that measurement missed was not how the view LOOKED — it was that the
// view could not be created at all. Driven at the real views.open on 2026-08-05 the shipped body was
// refused four ways at once (a `markdown` block, an alert whose text was a string, two `raw_number` cells).
// Every one of those is a shape the published pages permit and the live API does not.
//
// THE TRIGGER IS DELIBERATELY BOGUS, and that is what makes this a body test rather than a click test. A
// real trigger_id exists only inside the three seconds after a human clicks, which no test can hold. With
// an invalid one Slack still validates the view FIRST: a body it cannot parse answers `invalid_arguments`
// and names the offending json-pointer, and a body it accepts gets as far as complaining about the
// trigger. So `invalid_trigger_id` is this leg's PASS — it is Slack saying "the only thing wrong here is
// the thing I gave you deliberately".
//
// AND THE CONTROL BELOW IS NOT DECORATION: without it, a leg asserting `invalid_trigger_id` would go green
// on any API that checked the trigger before the body, and would then be certifying nothing at all.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

const deadTriggerID = "0000000000.0000000000.deadbeef"

func viewsOpen(t *testing.T, token []byte, body []byte) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://slack.com/api/views.open", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build the views.open request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("views.open: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode views.open's answer %q: %v", raw, err)
	}
	return out
}

func viewsOpenComplaint(answer map[string]any) string {
	var parts []string
	if meta, ok := answer["response_metadata"].(map[string]any); ok {
		for _, m := range meta["messages"].([]any) {
			parts = append(parts, m.(string))
		}
	}
	return strings.Join(parts, "; ")
}

// TestTheModalBodyIsAcceptedByTheLiveViewsOpen drives the SHIPPED builder.
func TestTheModalBodyIsAcceptedByTheLiveViewsOpen(t *testing.T) {
	token := []byte(os.Getenv("SLACK_BOT_TOKEN"))
	if len(token) == 0 {
		t.Skip("SLACK_BOT_TOKEN is unset — this leg needs a real workspace")
	}

	// The fixture exercises every block the modal can draw: the header, BOTH alerts (an expiry, and the
	// truncation warning that only fires on an oversized document), the leaf table with a nested object,
	// an array, a bare number and a boolean, and the deny-reason input. A fixture that drew fewer would
	// leave the untested shapes exactly where the four defects were found.
	req := slack.ApprovalRequest{
		ApprovalID: "apr_live", RequestHash: "rh_live",
		Identity:      "jira.transitionIssue",
		OperatorLabel: "Move a Jira issue to a new status",
		Arguments: []byte(`{"issue":"PAL-42","timeout_seconds":600,"notify":true,` +
			`"fields":{"assignee":"aylin","labels":["urgent","ios"]},"empty":{}}`),
		ExpiresAt: time.Now().Add(11 * time.Minute),
		Destination: []slack.ApprovalArgument{
			{Name: "repository", Value: "github.com/palgroup/centauri-ios.git"},
			{Name: "branch", Value: "agent/ws_986c86ee/run_e4036140"},
		},
	}
	body := slack.ToolApprovalModal(deadTriggerID, req)

	answer := viewsOpen(t, token, body)
	if answer["error"] != "invalid_trigger_id" {
		t.Fatalf("views.open answered %v (%s) for the SHIPPED modal body.\n"+
			"Anything other than invalid_trigger_id means Slack refused the VIEW, not the deliberately dead "+
			"trigger — so the Show-arguments button opens nothing, however correctly it is wired.\nbody: %s",
			answer["error"], viewsOpenComplaint(answer), body)
	}

	// THE CONTROL. If Slack checked the trigger before the body, the assertion above would pass over any
	// view at all — including one that is obviously broken. This proves the order.
	broken := []byte(`{"trigger_id":"` + deadTriggerID + `","view":{"type":"modal",` +
		`"title":{"type":"plain_text","text":"probe"},` +
		`"blocks":[{"type":"markdown","text":"a view refuses this block"}]}}`)
	control := viewsOpen(t, token, broken)
	if control["error"] != "invalid_arguments" {
		t.Fatalf("a view carrying a `markdown` block answered %v, not invalid_arguments — so this API does "+
			"not validate the body ahead of the trigger and the assertion above certifies nothing",
			control["error"])
	}
	t.Logf("shipped body -> %v; the broken control -> %v (%s)",
		answer["error"], control["error"], viewsOpenComplaint(control))
}

// TestTheChannelMessageSurvivesANumericArgument is the OTHER half of the raw_number defect, on the surface
// an operator actually depends on today.
//
// The renderer typed a cell as `raw_number` whenever the argument's JSON encoding was a bare number, and
// the live chat.postMessage refuses that cell — so a gated call carrying a timeout, a line count or a port
// posted NO approval message at all. The run parks, nobody is asked, and the only trace is one log line.
// Two calls, one control: the string-only document must post either way, so a regression in the numeric
// one cannot be read as the workspace being unreachable.
func TestTheChannelMessageSurvivesANumericArgument(t *testing.T) {
	token := []byte(os.Getenv("SLACK_BOT_TOKEN"))
	channel := os.Getenv("SLACK_TEST_CHANNEL")
	if len(token) == 0 || channel == "" {
		t.Skip("SLACK_BOT_TOKEN / SLACK_TEST_CHANNEL are unset — this leg needs a real workspace")
	}

	post := func(arguments string) map[string]any {
		body := slack.ToolApprovalMessage(channel, "", slack.ApprovalRequest{
			ApprovalID: "apr_live", RequestHash: "rh_live", Identity: "palai.workspace.shell",
			OperatorLabel: "Run a shell command", Arguments: []byte(arguments),
		})
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://slack.com/api/chat.postMessage", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+string(token))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("chat.postMessage: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}

	if control := post(`{"command":"swift build"}`); control["ok"] != true {
		t.Fatalf("the string-only approval message did not post (%v), so the numeric case below would be "+
			"measuring the workspace rather than the renderer", control)
	}
	if numeric := post(`{"command":"swift build","timeout_seconds":600,"max_lines":100}`); numeric["ok"] != true {
		t.Fatalf("an approval message carrying a NUMERIC argument was refused: %v.\n"+
			"That is a gated call whose question never reaches a human at all — the run parks server-side "+
			"and the thread simply stops.", numeric)
	}
}
