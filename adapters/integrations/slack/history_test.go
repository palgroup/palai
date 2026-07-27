package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every fixture below is built from the PUBLISHED reference, not from ThreadReplies' own decoder — a fake
// shaped by the code it tests confirms itself.
//
// CONTRACT: https://docs.slack.dev/reference/methods/conversations.replies/ (checked 2026-07-27) — GET;
// required `channel` + `ts`, optional `limit`; answers {ok, messages:[…], has_more,
// response_metadata:{next_cursor}}; a refusal is HTTP 200 carrying {"ok":false,"error":…}.

// repliesPeer serves one scripted body and records what was asked for.
func repliesPeer(t *testing.T, status int, body string) (base string, seen *http.Request, count *int) {
	t.Helper()
	var got http.Request
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		n++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &got, &n
}

// The call carries exactly the documented arguments, and the credential rides the header ONLY. A token in the
// query string is a token in every proxy log between here and Slack.
func TestThreadRepliesCarriesTheDocumentedArguments(t *testing.T) {
	base, seen, _ := repliesPeer(t, http.StatusOK, `{"ok":true,"messages":[],"has_more":false}`)

	if _, _, err := ThreadReplies(context.Background(), http.DefaultClient, base,
		[]byte("xoxb-not-a-credential"), "C1", "1700000001.000100", 100); err != nil {
		t.Fatalf("ThreadReplies: %v", err)
	}
	if seen.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET — the page documents GET and a read has no body", seen.Method)
	}
	if seen.URL.Path != "/conversations.replies" {
		t.Fatalf("path = %s, want /conversations.replies", seen.URL.Path)
	}
	q := seen.URL.Query()
	if q.Get("channel") != "C1" || q.Get("ts") != "1700000001.000100" || q.Get("limit") != "100" {
		t.Fatalf("query = %v, want the documented channel + ts + an EXPLICIT limit (the default is 1000)", q)
	}
	if auth := seen.Header.Get("Authorization"); auth != "Bearer xoxb-not-a-credential" {
		t.Fatalf("Authorization = %q, want the bot token as a bearer", auth)
	}
	if strings.Contains(seen.URL.RawQuery, "xoxb") {
		t.Fatalf("the token reached the query string: %s", seen.URL.RawQuery)
	}
}

// A missing channel, thread or limit refuses WITHOUT calling: an unbounded read is the documented default
// (limit=1000), so a caller that forgot the bound must not silently get it.
func TestThreadRepliesRefusesAnUnboundedOrUnaddressedRead(t *testing.T) {
	base, _, count := repliesPeer(t, http.StatusOK, `{"ok":true,"messages":[]}`)
	for _, bad := range []struct {
		channel, ts string
		limit       int
	}{
		{"", "1.1", 100}, {"C1", "", 100}, {"C1", "1.1", 0}, {"C1", "1.1", -1},
	} {
		if _, _, err := ThreadReplies(context.Background(), http.DefaultClient, base, nil,
			bad.channel, bad.ts, bad.limit); err == nil {
			t.Fatalf("channel=%q ts=%q limit=%d was accepted", bad.channel, bad.ts, bad.limit)
		}
	}
	if *count != 0 {
		t.Fatalf("the peer saw %d call(s); a refused read must not be sent", *count)
	}
}

// RETRACTION STAYS RETRACTED. Slack replaces a deleted thread parent with a `tombstone` subtype, and the app's
// own retraction work must not be undone from Slack's side. The rule is an allow-list of the EMPTY subtype:
// no page enumerates the subtype set, so anything else is dropped — including a join, a leave and a file share.
func TestThreadRepliesDropsSubtypedAndEmptyMessages(t *testing.T) {
	base, _, _ := repliesPeer(t, http.StatusOK, `{"ok":true,"has_more":false,"messages":[
		{"type":"message","subtype":"tombstone","text":"This message was deleted.","ts":"1.1"},
		{"type":"message","user":"U1","text":"the real question","ts":"1.2"},
		{"type":"message","subtype":"channel_join","user":"U2","text":"<@U2> has joined","ts":"1.3"},
		{"type":"message","user":"U2","text":"","ts":"1.4"},
		{"type":"message","subtype":"file_share","user":"U2","text":"see the log","ts":"1.5"},
		{"type":"message","user":"U3","text":"the answer","ts":"1.6"}]}`)

	msgs, hasMore, err := ThreadReplies(context.Background(), http.DefaultClient, base, nil, "C1", "1.1", 100)
	if err != nil {
		t.Fatalf("ThreadReplies: %v", err)
	}
	if hasMore {
		t.Fatal("has_more = true, want the page's own false")
	}
	if len(msgs) != 2 || msgs[0].Text != "the real question" || msgs[1].Text != "the answer" {
		t.Fatalf("messages = %+v, want only the two subtype-free, non-empty ones — a tombstone is a RETRACTION and must not come back through this path", msgs)
	}
	if msgs[0].UserID != "U1" || msgs[0].TS != "1.2" {
		t.Fatalf("messages[0] = %+v, want the author and ts carried (the caller needs both: one to tell OUR turns apart, one to skip the current one)", msgs[0])
	}
}

// has_more is the page's own word and is passed up, so the caller can SAY the thread continues rather than
// implying it ended where the page did.
func TestThreadRepliesSurfacesHasMore(t *testing.T) {
	base, _, _ := repliesPeer(t, http.StatusOK,
		`{"ok":true,"has_more":true,"messages":[{"type":"message","user":"U1","text":"one","ts":"1.1"}],"response_metadata":{"next_cursor":"c"}}`)
	_, hasMore, err := ThreadReplies(context.Background(), http.DefaultClient, base, nil, "C1", "1.1", 1)
	if err != nil || !hasMore {
		t.Fatalf("hasMore = %v (err %v), want the page's own true", hasMore, err)
	}
}

// A refusal is TYPED, so a caller can tell `missing_scope` (a private channel this app was never granted) from
// `thread_not_found` (the thread is gone) without matching substrings of an error message.
func TestThreadRepliesTypesTheAPIRefusal(t *testing.T) {
	for _, code := range []string{"missing_scope", "thread_not_found", "channel_not_found"} {
		base, _, _ := repliesPeer(t, http.StatusOK, `{"ok":false,"error":"`+code+`"}`)
		_, _, err := ThreadReplies(context.Background(), http.DefaultClient, base, nil, "C1", "1.1", 100)
		if got := APIErrorCode(err); got != code {
			t.Fatalf("APIErrorCode = %q, want %q (err %v)", got, code, err)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("error %v is not an *APIError", err)
		}
	}
}

// A 429 is an ERROR, not a retry. PostMessage owns this package's single repair budget and it exists for the
// VISIBLE message; a read runs inside the 2-second Slack ack budget, so retrying here would spend the budget
// that has to birth the run. Exactly one round trip.
func TestThreadRepliesDoesNotRetryARateLimit(t *testing.T) {
	base, _, count := repliesPeer(t, http.StatusTooManyRequests, `{"ok":false,"error":"ratelimited"}`)
	if _, _, err := ThreadReplies(context.Background(), http.DefaultClient, base, nil, "C1", "1.1", 100); err == nil {
		t.Fatal("a 429 answered without an error")
	}
	if *count != 1 {
		t.Fatalf("the peer saw %d call(s), want exactly 1 — a read must not spend the ack budget on repairs", *count)
	}
}
