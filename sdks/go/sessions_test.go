package palai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestCreateSessionPostsToV1Sessions(t *testing.T) {
	var gotPath, gotBody string
	var gotIdempotencyKey string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotIdempotencyKey = r.Header.Get("Idempotency-Key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResp(201, `{"id":"sess_1","object":"session","status":"open"}`), nil
	})
	c := testClient(t, rt)

	s, err := c.CreateSession(context.Background(), CreateSessionParams{Name: "triage"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if gotPath != "/v1/sessions" {
		t.Fatalf("path = %q, want /v1/sessions", gotPath)
	}
	if !strings.Contains(gotBody, `"name":"triage"`) {
		t.Fatalf("body %q does not carry name", gotBody)
	}
	if gotIdempotencyKey != "" {
		t.Fatalf("CreateSession must not send an Idempotency-Key (a retried create mints a new session), got %q", gotIdempotencyKey)
	}
	if s.ID != "sess_1" || s.Object != "session" || s.Status != "open" {
		t.Fatalf("session = %+v, want id=sess_1 object=session status=open", s)
	}
}

// TestCreateSessionDoesNotRetryOnNetworkFailure locks the doc comment's claim: unlike
// Responses.Create, a torn connection is NOT retried, because a resend would mint a second
// session rather than settle the same one.
func TestCreateSessionDoesNotRetryOnNetworkFailure(t *testing.T) {
	attempts := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("network unreachable")
	})
	c := testClient(t, rt)

	_, err := c.CreateSession(context.Background(), CreateSessionParams{Name: "x"})
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("want *ConnectionError, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("CreateSession must fail closed with a single attempt, got %d", attempts)
	}
}

func TestSteerSessionPostsDurableCommand(t *testing.T) {
	var gotPath, gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResp(202, `{"id":"cmd_1","object":"command","session_id":"sess_1","kind":"send_message","delivery":"steer","status":"queued"}`), nil
	})
	c := testClient(t, rt)

	cmd, err := c.SteerSession(context.Background(), "sess_1", SteerParams{Message: "keep going"})
	if err != nil {
		t.Fatalf("SteerSession: %v", err)
	}
	if gotPath != "/v1/sessions/sess_1/commands" {
		t.Fatalf("path = %q, want /v1/sessions/sess_1/commands", gotPath)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, gotBody)
	}
	if body["kind"] != "send_message" || body["delivery"] != "steer" || body["message"] != "keep going" {
		t.Fatalf("body = %s, want kind=send_message delivery=steer message=%q", gotBody, "keep going")
	}
	if cid, _ := body["command_id"].(string); cid == "" {
		t.Fatalf("body %s carries no command_id", gotBody)
	}
	if cmd.ID != "cmd_1" || cmd.Status != "queued" || cmd.SessionID != "sess_1" {
		t.Fatalf("command = %+v, want id=cmd_1 status=queued session_id=sess_1", cmd)
	}
}

// TestSteerSessionEscapesSessionIDInPath guards the tree's recorded path-handling defect family: an
// id containing a "/" must not be spliced unescaped into the URL path (which would change which
// resource is addressed).
func TestSteerSessionEscapesSessionIDInPath(t *testing.T) {
	var gotPath string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.EscapedPath()
		return jsonResp(202, `{"id":"cmd_1","object":"command","session_id":"a/b","kind":"send_message","status":"queued"}`), nil
	})
	c := testClient(t, rt)

	if _, err := c.SteerSession(context.Background(), "a/b", SteerParams{Message: "hi"}); err != nil {
		t.Fatalf("SteerSession: %v", err)
	}
	if gotPath != "/v1/sessions/a%2Fb/commands" {
		t.Fatalf("path = %q, want the session id percent-escaped (a%%2Fb), not spliced raw", gotPath)
	}
}

// TestSteerSessionUsesCallerCommandID: a caller-supplied CommandID rides verbatim, so a caller can
// dedupe a steer across its own retried processes (not just this SDK's transport retry).
func TestSteerSessionUsesCallerCommandID(t *testing.T) {
	var gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResp(202, `{"id":"cmd_1","object":"command","status":"queued"}`), nil
	})
	c := testClient(t, rt)

	if _, err := c.SteerSession(context.Background(), "sess_1", SteerParams{Message: "hi", CommandID: "cmd_caller_1"}); err != nil {
		t.Fatalf("SteerSession: %v", err)
	}
	if !strings.Contains(gotBody, `"command_id":"cmd_caller_1"`) {
		t.Fatalf("body %q did not carry the caller's command_id", gotBody)
	}
}

// TestSteerSessionRetriesNetworkFailureWithSameCommandID: SteerSession is marked idempotent
// (command_id is the server's own dedupe key, spec §22.4), so a torn connection is retried, and the
// SAME command_id rides every attempt — a different one per attempt would let a resend settle as a
// second command.
func TestSteerSessionRetriesNetworkFailureWithSameCommandID(t *testing.T) {
	var mu sync.Mutex
	var commandIDs []string
	attempt := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempt++
		n := attempt
		mu.Unlock()
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		commandIDs = append(commandIDs, body["command_id"].(string))
		mu.Unlock()
		if n <= 2 {
			return nil, errors.New("network unreachable")
		}
		return jsonResp(202, `{"id":"cmd_1","object":"command","status":"queued"}`), nil
	})
	c := testClient(t, rt)

	if _, err := c.SteerSession(context.Background(), "sess_1", SteerParams{Message: "hi"}); err != nil {
		t.Fatalf("SteerSession: %v", err)
	}
	if len(commandIDs) != 3 {
		t.Fatalf("two network failures then success is three attempts, got %d", len(commandIDs))
	}
	for _, id := range commandIDs {
		if id == "" || id != commandIDs[0] {
			t.Fatalf("command_id must be identical and non-empty across retries: %v", commandIDs)
		}
	}
}
