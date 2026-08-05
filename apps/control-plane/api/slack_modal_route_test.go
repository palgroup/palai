package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// showArgumentsPayload is interactionPayload plus the ONE field the modal path needs and the decision path
// never reads.
//
// CONTRACT: https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/ (checked
// 2026-07-26) — a block_actions payload carries `trigger_id`.
func showArgumentsPayload(team, user, channel, messageTS, triggerID, value string) []byte {
	var body map[string]any
	_ = json.Unmarshal(interactionPayload(team, user, channel, messageTS, slack.ActionShowArguments, value), &body)
	body["trigger_id"] = triggerID
	raw, _ := json.Marshal(body)
	return raw
}

// THE THREE-SECOND RULE, MEASURED RATHER THAN COMMENTED (E23 T4, plan §3.5 P10 + E19).
//
// Two deadlines run from the same instant: Slack expires `trigger_id` three seconds after sending it, and
// this route owes Slack a 200 in the same three seconds. The only way both can hold is for views.open to be
// called INSIDE the ack budget, which is what this asserts — not by timing a fast machine, but by reading
// the DEADLINE the route handed the call. A handler that deferred the modal to a goroutine would pass a
// background context with no deadline, and that is exactly what fails here.
func TestShowArgumentsOpensTheModalInsideTheAckBudgetAndDecidesNothing(t *testing.T) {
	secret := []byte("test-signing-secret")
	decider := newSlackDecider(secret)
	peer, done := interactionPeerAgainst(t, decider, secret)
	defer done()

	before := time.Now()
	resp := peer.postForm(t, showArgumentsPayload("T100", "Umapped", "C1", "1700000000.000100", "tr.42", "req_hash"), time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a Show-arguments click = %d, want 200", resp.StatusCode)
	}

	// THE CLAIM THAT MATTERS MOST: a document was opened and NOTHING was decided. Decide is not in the call
	// list — not called and refused, not called at all.
	if want := []string{"resolve", "verify", "open"}; !equalStrings(decider.calls, want) {
		t.Fatalf("handler order = %v, want %v — opening arguments must never reach the decision path", decider.calls, want)
	}
	if len(decider.intents) != 0 {
		t.Fatalf("the modal branch produced %d approval intent(s): %+v", len(decider.intents), decider.intents)
	}

	if len(decider.opens) != 1 {
		t.Fatalf("%d modal opens, want 1", len(decider.opens))
	}
	got := decider.opens[0]
	if got.TriggerID != "tr.42" || got.RequestHash != "req_hash" || got.UserID != "Umapped" ||
		got.ChannelID != "C1" || got.ThreadTS != "1700000000.000100" {
		t.Fatalf("the modal intent lost a binding: %+v", got)
	}

	deadline := decider.openDeadlines[0]
	if deadline.IsZero() {
		t.Fatal("views.open was handed a context with NO deadline; the trigger_id dies in three seconds and this path is not inside a budget at all")
	}
	if budget := deadline.Sub(before); budget > 3*time.Second {
		t.Fatalf("views.open ran under a %v budget; the trigger_id expires after 3s and the ack is owed in 3s", budget)
	}
}

// A STALLED MODAL IS AN HONEST FAILURE, NOT A HANG. The budget bounds the whole handler, so a ledger read
// that never returns costs the human their document and costs Slack nothing — the route still answers.
func TestAStalledModalStillAnswersSlackInsideTheBudget(t *testing.T) {
	secret := []byte("test-signing-secret")
	decider := newSlackDecider(secret)
	decider.openHang = true
	peer, done := interactionPeerAgainst(t, decider, secret)
	defer done()

	start := time.Now()
	resp := peer.postForm(t, showArgumentsPayload("T100", "Umapped", "C1", "1700000000.000100", "tr.42", "req_hash"), time.Now())
	defer resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a stalled modal = %d, want 503", resp.StatusCode)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the route took %v to answer a stalled modal; Slack is owed 200 within 3 seconds", elapsed)
	}
}

// A REFUSAL OPENS NOTHING AND SAYS NOTHING, the same asymmetry a refused decision has: a readable refusal
// is a signal an unmapped user can probe for. The clicker gets a 200 and no modal.
func TestARefusedModalIsAcknowledgedWithoutTellingTheClicker(t *testing.T) {
	secret := []byte("test-signing-secret")
	decider := newSlackDecider(secret)
	decider.openRejected = "the clicking user is not an authorized approver"
	peer, done := interactionPeerAgainst(t, decider, secret)
	defer done()

	resp := peer.postForm(t, showArgumentsPayload("T100", "Uunmapped", "C1", "1700000000.000100", "tr.42", "req_hash"), time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a refused modal = %d, want 200 with nothing opened", resp.StatusCode)
	}
	var problem map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&problem)
	if len(problem) != 0 {
		t.Fatalf("the refusal was described to the clicker: %v", problem)
	}
}

// A Show-arguments click with NO trigger_id is not a modal request, so it falls through to the approval
// mapping — which refuses it too, because the action id is not approve or deny. Nothing opens, nothing
// decides, and Slack still gets its 200.
func TestAShowArgumentsClickWithoutATriggerOpensNothingAndDecidesNothing(t *testing.T) {
	secret := []byte("test-signing-secret")
	decider := newSlackDecider(secret)
	peer, done := interactionPeerAgainst(t, decider, secret)
	defer done()

	payload := interactionPayload("T100", "Umapped", "C1", "1700000000.000100", slack.ActionShowArguments, "req_hash")
	resp := peer.postForm(t, payload, time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a triggerless click = %d, want 200", resp.StatusCode)
	}
	if want := []string{"resolve", "verify"}; !equalStrings(decider.calls, want) {
		t.Fatalf("handler order = %v, want %v — neither surface should have been reached", decider.calls, want)
	}
}
