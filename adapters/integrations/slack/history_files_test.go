package slack

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fileThreadDoer struct{ body string }

func (d fileThreadDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(d.body))}, nil
}

// THE BUG THIS HOLDS, found live: a human posted a screenshot in one message and the @mention in the next.
// The run was born from the message with no attachment, and the thread read returned the screenshot's
// CAPTION and dropped the file — so the agent answered "I cannot see the image" about an image that was
// right there. Reading a thread as text alone makes a picture invisible.
func TestThreadRepliesCarriesTheFilesAMessageShared(t *testing.T) {
	doer := fileThreadDoer{body: `{"ok":true,"has_more":false,"messages":[
		{"user":"U1","ts":"1.0","text":"ne yazıyor kral","files":[
			{"id":"F1","name":"image.png","mimetype":"image/png","size":1234,
			 "url_private_download":"https://files.slack.test/F1"}]},
		{"user":"U1","ts":"2.0","text":"sana diyorum"}]}`}

	msgs, _, err := ThreadReplies(context.Background(), doer, "https://slack.test/api", []byte("t"), "C1", "1.0", 20)
	if err != nil {
		t.Fatalf("ThreadReplies: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if len(msgs[0].Files) != 1 {
		t.Fatal("the thread message's file was dropped — this is exactly the shape that made a shared " +
			"screenshot invisible to the run")
	}
	f := msgs[0].Files[0]
	if f.ID != "F1" || f.MimeType != "image/png" || f.DownloadURL != "https://files.slack.test/F1" {
		t.Fatalf("decoded file = %+v, want the id, mimetype and download url", f)
	}
	if len(msgs[1].Files) != 0 {
		t.Fatalf("a message with no files reported %d", len(msgs[1].Files))
	}
}

// A caption-less image is still a message worth carrying: the old filter dropped anything with empty text,
// which would have thrown away a bare screenshot — the single most likely way to share one.
func TestACaptionlessImageIsNotDropped(t *testing.T) {
	doer := fileThreadDoer{body: `{"ok":true,"messages":[
		{"user":"U1","ts":"1.0","text":"","files":[
			{"id":"F9","mimetype":"image/png","size":10,"url_private_download":"https://files.slack.test/F9"}]}]}`}
	msgs, _, err := ThreadReplies(context.Background(), doer, "https://slack.test/api", []byte("t"), "C1", "1.0", 20)
	if err != nil {
		t.Fatalf("ThreadReplies: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Files) != 1 {
		t.Fatalf("a caption-less image was dropped: %+v", msgs)
	}
}

// url_private is the fallback when the download form is absent — a file object carrying only the
// browser-rendering url is still fetchable with the same token.
func TestAFileWithOnlyURLPrivateIsStillFetchable(t *testing.T) {
	doer := fileThreadDoer{body: `{"ok":true,"messages":[
		{"user":"U1","ts":"1.0","text":"x","files":[
			{"id":"F2","mimetype":"image/png","size":10,"url_private":"https://files.slack.test/F2"}]}]}`}
	msgs, _, _ := ThreadReplies(context.Background(), doer, "https://slack.test/api", []byte("t"), "C1", "1.0", 20)
	if len(msgs) != 1 || len(msgs[0].Files) != 1 || msgs[0].Files[0].DownloadURL != "https://files.slack.test/F2" {
		t.Fatalf("url_private was not used as the fallback: %+v", msgs)
	}
}

// A subtyped message stays dropped, files and all: that rule is what keeps a RETRACTION retracted, and a
// tombstone's attachments are no more readable than its text.
func TestASubtypedMessagesFilesAreStillDropped(t *testing.T) {
	doer := fileThreadDoer{body: `{"ok":true,"messages":[
		{"user":"U1","ts":"1.0","subtype":"tombstone","text":"gone","files":[
			{"id":"F3","mimetype":"image/png","size":10,"url_private_download":"https://files.slack.test/F3"}]}]}`}
	msgs, _, _ := ThreadReplies(context.Background(), doer, "https://slack.test/api", []byte("t"), "C1", "1.0", 20)
	if len(msgs) != 0 {
		t.Fatalf("a subtyped message survived with %d file(s) — a retraction must stay retracted", len(msgs[0].Files))
	}
}
