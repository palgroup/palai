package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A shared image must reach the Event as file METADATA. SLK-005 classified the share and stopped there;
// without the file's identity there is nothing downstream to fetch.
func TestMapEventCarriesSharedImageFiles(t *testing.T) {
	body := []byte(`{"type":"event_callback","team_id":"T1","event_id":"EvF","event":{
		"type":"message","subtype":"file_share","user":"U1","channel":"C1","ts":"1.1",
		"text":"ne yazıyor burada",
		"files":[{"id":"F1","name":"shot.png","mimetype":"image/png","filetype":"png","size":1234,
		          "url_private_download":"https://files.slack.com/files-pri/T1-F1/download/shot.png"}]}}`)
	ev, err := MapEvent(body, "Ubot", false)
	if err != nil {
		t.Fatalf("MapEvent: %v", err)
	}
	if ev.Kind != KindFileShare {
		t.Fatalf("kind = %q, want file_share", ev.Kind)
	}
	if len(ev.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(ev.Files))
	}
	f := ev.Files[0]
	if f.ID != "F1" || f.MimeType != "image/png" || f.Size != 1234 {
		t.Fatalf("file = %+v, want the declared id/mimetype/size", f)
	}
	if f.DownloadURL != "https://files.slack.com/files-pri/T1-F1/download/shot.png" {
		t.Fatalf("download url = %q, want url_private_download", f.DownloadURL)
	}
	if ev.Text != "ne yazıyor burada" {
		t.Fatalf("text = %q, want the comment that came with the file", ev.Text)
	}
}

// Files must be read whatever the subtype says. An @mention WITH an attachment arrives as app_mention (no
// subtype at all) and a DM upload arrives as message.im; keying the parse on subtype "file_share" would
// see neither, which is the exact shape a "we handle file shares" claim would be false about.
func TestMapEventCarriesFilesRegardlessOfSubtype(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"an app_mention with an attachment", `{"type":"event_callback","team_id":"T1","event_id":"Ev1","event":{
			"type":"app_mention","user":"U1","channel":"C1","ts":"1.1","text":"<@Ubot> bak",
			"files":[{"id":"F1","mimetype":"image/png","url_private_download":"https://files.slack.com/x"}]}}`},
		{"a DM upload with no subtype", `{"type":"event_callback","team_id":"T1","event_id":"Ev2","event":{
			"type":"message","channel_type":"im","user":"U1","channel":"D1","ts":"2.2",
			"files":[{"id":"F2","mimetype":"image/jpeg","url_private_download":"https://files.slack.com/y"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := MapEvent([]byte(tc.body), "Ubot", false)
			if err != nil {
				t.Fatalf("MapEvent: %v", err)
			}
			if len(ev.Files) != 1 {
				t.Fatalf("files = %d, want 1 — a file is a file whatever the subtype claims", len(ev.Files))
			}
		})
	}
}

// A file-less event must carry no Files, so every existing event shape is untouched.
func TestMapEventCarriesNoFilesWhenNoneWereShared(t *testing.T) {
	ev, err := MapEvent([]byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev9","event":{"type":"app_mention","user":"U1","channel":"C1","ts":"9.9","text":"merhaba"}}`), "Ubot", false)
	if err != nil {
		t.Fatalf("MapEvent: %v", err)
	}
	if len(ev.Files) != 0 {
		t.Fatalf("files = %+v, want none", ev.Files)
	}
}

// doerFunc adapts a function to Doer.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// respond builds a canned 200 with the given bytes.
func respond(body []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}
}

// pngBytes is a real 1x1 PNG header, enough for http.DetectContentType to sniff image/png.
var pngBytes = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89,
}

// The fetch carries the bot token in the Authorization header and NOWHERE else, and it sniffs the media
// type from the BYTES rather than trusting what the payload declared.
//
// CONTRACT: https://docs.slack.dev/messaging/working-with-files/ (checked 2026-07-27) — a private file is
// downloaded from its url_private_download with `Authorization: Bearer <token>` and the `files:read` scope.
func TestFetchImageUsesTheTokenOnlyAsABearerHeader(t *testing.T) {
	var seen *http.Request
	got, err := FetchImage(context.Background(), doerFunc(func(r *http.Request) (*http.Response, error) {
		seen = r
		return respond(pngBytes), nil
	}), []byte("xoxb-SECRET"), SharedFile{ID: "F1", MimeType: "image/png", DownloadURL: "https://files.slack.com/files-pri/T1-F1/download/x.png"}, 1<<20)
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if got.MediaType != "image/png" || len(got.Content) != len(pngBytes) {
		t.Fatalf("fetched = %s/%d bytes, want image/png and the full body", got.MediaType, len(got.Content))
	}
	if seen.Header.Get("Authorization") != "Bearer xoxb-SECRET" {
		t.Fatalf("Authorization = %q, want the bearer header", seen.Header.Get("Authorization"))
	}
	if seen.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", seen.Method)
	}
	if strings.Contains(seen.URL.String(), "xoxb") {
		t.Fatalf("url = %q carries the token — a credential in a URL lands in every proxy log there is", seen.URL)
	}
}

// A DECLARED image/png whose bytes are not an image must be refused. The mimetype and the filename come
// from the uploader; only the bytes are evidence.
func TestFetchImageRefusesBytesThatAreNotAnImage(t *testing.T) {
	_, err := FetchImage(context.Background(), doerFunc(func(*http.Request) (*http.Response, error) {
		return respond([]byte("#!/bin/sh\nrm -rf /\n")), nil
	}), []byte("t"), SharedFile{ID: "F1", MimeType: "image/png", Name: "totally-a.png", DownloadURL: "https://files.slack.com/x"}, 1<<20)
	if !errors.Is(err, ErrNotAnImage) {
		t.Fatalf("err = %v, want ErrNotAnImage", err)
	}
}

// A body over the ceiling is refused rather than buffered. Unbounded is a memory attack and a provider
// bill, and the limit has to bite on the RESPONSE — a declared `size` is the uploader's word.
func TestFetchImageRefusesABodyOverTheCeiling(t *testing.T) {
	big := make([]byte, 4096)
	copy(big, pngBytes)
	_, err := FetchImage(context.Background(), doerFunc(func(*http.Request) (*http.Response, error) {
		return respond(big), nil
	}), []byte("t"), SharedFile{ID: "F1", MimeType: "image/png", DownloadURL: "https://files.slack.com/x"}, 1024)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge for a 4096-byte body under a 1024-byte ceiling", err)
	}
}

// A declared size over the ceiling is refused BEFORE the request is made — the cheap half of the same
// bound, and the one that avoids paying for the transfer at all.
func TestFetchImageRefusesADeclaredSizeOverTheCeilingWithoutFetching(t *testing.T) {
	called := false
	_, err := FetchImage(context.Background(), doerFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return respond(pngBytes), nil
	}), []byte("t"), SharedFile{ID: "F1", MimeType: "image/png", Size: 99999, DownloadURL: "https://files.slack.com/x"}, 1024)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
	if called {
		t.Fatal("the fetch happened anyway — a declared oversize must be refused before the transfer")
	}
}

// THE CONFUSED-DEPUTY GUARD. The bot holds files:read, so a fetch is a read primitive: the ONLY hosts the
// token may be presented to are Slack's own file hosts. A payload is signature-verified, so today the URL
// is Slack's — but the guard is what makes that a property of this code rather than a property of Slack
// never changing a field, and it is the same rule E20 T3 spent a whole task on for context entities.
func TestFetchImageRefusesAnyHostButSlackFiles(t *testing.T) {
	for _, url := range []string{
		"https://evil.example.com/x.png",
		"http://files.slack.com/x.png",                   // plaintext would put the bearer on the wire
		"https://files.slack.com.evil.example.com/x.png", // suffix trick
		"https://filesXslack.com/x.png",
		"https://169.254.169.254/latest/meta-data/",
		"",
	} {
		called := false
		_, err := FetchImage(context.Background(), doerFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return respond(pngBytes), nil
		}), []byte("t"), SharedFile{ID: "F1", MimeType: "image/png", DownloadURL: url}, 1<<20)
		if !errors.Is(err, ErrUntrustedFileHost) {
			t.Fatalf("url %q: err = %v, want ErrUntrustedFileHost", url, err)
		}
		if called {
			t.Fatalf("url %q: the token was presented to a non-Slack host", url)
		}
	}
}

// A non-2xx must not become an "image" made of the error page's bytes.
func TestFetchImageRefusesANonOKResponse(t *testing.T) {
	_, err := FetchImage(context.Background(), doerFunc(func(*http.Request) (*http.Response, error) {
		r := respond([]byte(`{"error":"not_allowed"}`))
		r.StatusCode = http.StatusForbidden
		return r, nil
	}), []byte("t"), SharedFile{ID: "F1", MimeType: "image/png", DownloadURL: "https://files.slack.com/x"}, 1<<20)
	if err == nil {
		t.Fatal("a 403 was accepted as an image")
	}
	if strings.Contains(err.Error(), "t") && strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("err = %v carries credential material", err)
	}
}

// ImageCandidates is the admission filter: only files DECLARED as an image type are even considered, and
// the answer is stable and bounded so the run input it feeds is a pure function of the event.
func TestImageCandidatesKeepsOnlyDeclaredImagesAndBoundsTheCount(t *testing.T) {
	files := []SharedFile{
		{ID: "F1", MimeType: "image/png", DownloadURL: "u"},
		{ID: "F2", MimeType: "application/pdf", DownloadURL: "u"},
		{ID: "F3", MimeType: "image/jpeg", DownloadURL: "u"},
		{ID: "F4", MimeType: "image/gif", DownloadURL: "u"},
		{ID: "F5", MimeType: "image/webp", DownloadURL: "u"},
		{ID: "F6", MimeType: "image/png", DownloadURL: "u"},
		{ID: "F7", MimeType: "image/png", DownloadURL: ""}, // no download url: nothing to fetch
	}
	kept, skipped := ImageCandidates(files, 3, 1<<20)
	if len(kept) != 3 {
		t.Fatalf("kept = %d (%+v), want the cap of 3", len(kept), kept)
	}
	if kept[0].ID != "F1" || kept[1].ID != "F3" || kept[2].ID != "F4" {
		t.Fatalf("kept = %+v, want the first three declared images in payload order", kept)
	}
	// F2 (not an image), F5+F6 (over the cap), F7 (no url) all count as not-attached.
	if skipped != 4 {
		t.Fatalf("skipped = %d, want 4", skipped)
	}
}

// A DECLARED oversize must not consume one of the count slots. This was a real bug: the cap ran before the
// size check, so a message carrying one 50 MiB "image" and three screenshots delivered two of the three.
func TestImageCandidatesDoesNotSpendASlotOnADeclaredOversizeFile(t *testing.T) {
	files := []SharedFile{
		{ID: "HUGE", MimeType: "image/png", Size: 50 << 20, DownloadURL: "u"},
		{ID: "F1", MimeType: "image/png", Size: 100, DownloadURL: "u"},
		{ID: "F2", MimeType: "image/png", Size: 100, DownloadURL: "u"},
		{ID: "F3", MimeType: "image/png", Size: 100, DownloadURL: "u"},
	}
	kept, skipped := ImageCandidates(files, 3, 5<<20)
	if len(kept) != 3 || kept[0].ID != "F1" || kept[2].ID != "F3" {
		t.Fatalf("kept = %+v, want all three fetchable screenshots — the oversize file must not spend a slot", kept)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the oversize file)", skipped)
	}
}
