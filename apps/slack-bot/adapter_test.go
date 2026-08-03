// This file proves the seam Task 6 (2026-08-03 plan) exists to lock: apps/slack-bot, the module,
// can reach adapters/integrations/slack from exactly where it stands — adapter and bot are in the
// SAME Go module (github.com/palgroup/palai), so no move is needed for the bot to use it. The import
// surface was measured empty of Palai dependencies (`grep -h 'palai/' adapters/integrations/slack/*.go
// | grep -v _test | grep '"'` → nothing) before this test was written; that emptiness is what makes
// the adapter portable, and it is what this test exercises, not just imports.
//
// This is not a copy of adapters/integrations/slack/signature_test.go: that suite checks the
// adapter's own HMAC against its own unexported signatureFor helper. This one signs a request the
// way Slack's docs specify, using only stdlib crypto and the adapter's PUBLIC surface, to prove a
// caller outside the adapter package — the bot — gets the real, documented behavior: a genuine v0
// signature verifies, and a single tampered byte in the body does not.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

func TestBotCanVerifyASlackSignatureThroughTheAdapter(t *testing.T) {
	secret := []byte("a-signing-secret-only-the-bot-and-slack-know")
	body, err := json.Marshal(map[string]string{"type": "event_callback", "event_id": "Ev01"})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	timestamp := strconv.FormatInt(now.Unix(), 10)

	// Sign exactly as Slack's "Verifying requests from Slack" doc specifies: HMAC-SHA256 over the
	// base string "v0:{timestamp}:{body}". This does not call the adapter's unexported signatureFor
	// — it is an independent computation, so a passing VerifySignature here means the two agree.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	signature := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if err := slack.VerifySignature(secret, timestamp, signature, body, now, slack.DefaultTolerance); err != nil {
		t.Fatalf("genuine signature rejected: %v", err)
	}

	// A single changed byte in the body must break the MAC: the signature was computed over the
	// original body, so replaying it against a different body has to fail closed.
	tampered, err := json.Marshal(map[string]string{"type": "event_callback", "event_id": "Ev99"})
	if err != nil {
		t.Fatalf("marshal tampered body: %v", err)
	}
	if err := slack.VerifySignature(secret, timestamp, signature, tampered, now, slack.DefaultTolerance); !errors.Is(err, slack.ErrBadSignature) {
		t.Fatalf("tampered body: err = %v, want ErrBadSignature", err)
	}
}
