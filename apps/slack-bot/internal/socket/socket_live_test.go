//go:build live

// The bot's OWN Socket Mode loop against the real Slack server (2026-08-03 plan, Task 12.5).
//
// WHY THIS EXISTS WHEN tests/live/slack/socket_live_test.go ALREADY DOES. That one drives the raw protocol
// by hand — it dials, reads frames and writes acknowledgements itself — to check the published page
// against the live server. This one drives THIS PACKAGE's Run: the state machine an operator's bot
// actually runs. A fake peer built from the same page our loop was written from can only ever confirm that
// the two agree with each other; only the real server settles that a real app-level token opens a real
// connection and that our first frame handling is right about what arrives on it.
//
// WHY IT TAKES ITS TOKEN FROM THE ENVIRONMENT. It was written when it was this process's only live proof:
// the bot could not redeem its own handles then, so `go run ./apps/slack-bot` stopped before it dialled.
// That premise is gone — the process redeems over GET /v1/bots/{bot_id}/credentials
// (apps/slack-bot/credentials.go) and `slack-bot selftest <channel-id>` drives four live legs through a real
// row, this dial among them (apps/slack-bot/selftest.go). What this test still holds alone is a dial that
// needs no row, no API key and no control plane: a token handed straight to the TEST. It reads
// SLACK_APP_TOKEN from the environment of the machine RUNNING IT, which is exactly what the shipped process
// may never do and what a credential-gated test is for. TestNoSlackCredentialComesFromTheEnvironment
// (apps/slack-bot/wiring_test.go) enforces that split by skipping _test.go files and nothing else.
//
// It SKIPS rather than fails when the credential is absent, so a machine without one reports partial-green
// instead of a red wall.
package socket

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLiveBotSocketConnectsAndShutsDownCleanly holds a REAL Socket Mode connection open with a REAL
// app-level token, then shuts it down the way SIGTERM does.
//
// WHAT IT SETTLES that the fake peer cannot:
//
//   - a real xapp- token is accepted by apps.connections.open as this loop presents it (Bearer, form
//     content type, no parameters) — the shape is copied from a page, and a page can be stale;
//   - the real server's first frame is `hello`, which is what this loop logs a connection from;
//   - Run STAYS UP: it does not return while the connection is healthy, which is the difference between a
//     bot and a one-shot;
//   - cancelling the context returns nil promptly rather than hanging on a live socket.
func TestLiveBotSocketConnectsAndShutsDownCleanly(t *testing.T) {
	token := os.Getenv("SLACK_APP_TOKEN")
	if token == "" {
		t.Skip("SLACK_APP_TOKEN is not set — supply the app-level token (App → Basic Information → App-Level Tokens, connections:write) and re-run; no code changes are needed")
	}

	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{AppToken: []byte(token), Logf: logf}, &liveHandler{})
	}()

	// The connection is up when the loop has logged Slack's own hello.
	deadline := time.Now().Add(30 * time.Second)
	connected := false
	for time.Now().Before(deadline) && !connected {
		mu.Lock()
		for _, l := range lines {
			if strings.Contains(l, "Socket Mode connected") {
				connected = true
			}
		}
		mu.Unlock()
		select {
		case err := <-done:
			t.Fatalf("Run returned before the connection came up: %v (log: %v)", err, lines)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !connected {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no Socket Mode connection within 30s; the loop logged %v", lines)
	}

	// STAYING UP is the property, so give it a few seconds of doing nothing and assert it is still there.
	select {
	case err := <-done:
		t.Fatalf("Run returned while the connection was healthy: %v", err)
	case <-time.After(3 * time.Second):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run on shutdown returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of cancellation; shutdown must not hang on a live socket")
	}
}

// TestLiveBotSocketRefusesABadToken checks the OTHER half of the dial decision against the real server:
// Slack's own error code for a token it will not accept, and that this loop treats it as permanent rather
// than retrying a wall forever. The code is asserted rather than assumed — permanentOpenError's list is
// the one place this package guesses at Slack's vocabulary.
func TestLiveBotSocketRefusesABadToken(t *testing.T) {
	if os.Getenv("SLACK_APP_TOKEN") == "" {
		t.Skip("SLACK_APP_TOKEN is not set — this test needs a live Slack to answer, not a valid credential")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := Run(ctx, Config{AppToken: []byte("xapp-1-not-a-real-token"), Logf: func(string, ...any) {}}, &liveHandler{})
	if err == nil {
		t.Fatal("Run accepted a bogus app-level token")
	}
	if !strings.Contains(err.Error(), "invalid_auth") && !strings.Contains(err.Error(), "not_allowed_token_type") {
		t.Fatalf("Slack refused a bogus token with %v; permanentOpenError's list does not cover it, so this bot would retry a wall forever", err)
	}
}

// liveHandler records nothing and asserts nothing: these tests are about the CONNECTION, and an envelope
// only arrives if somebody happens to be typing in the workspace while they run.
type liveHandler struct{}

func (liveHandler) OnEventsAPI(context.Context, json.RawMessage)   {}
func (liveHandler) OnInteractive(context.Context, json.RawMessage) {}
