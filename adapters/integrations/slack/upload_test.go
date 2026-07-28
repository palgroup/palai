package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// quickTimeBytes is the first 12 bytes of a QuickTime container: a box of length 0x14 whose type is `ftyp`
// and whose MAJOR BRAND is `qt  `. This is what `simctl io recordVideo` writes — INCLUDING with
// `--codec=h264` — and it is the whole reason the extension is derived from content here.
//
// CONTRACT (MEASURED on this machine, 2026-07-28, plan §3.5 X2): a file written by
// `xcrun simctl io booted recordVideo --codec=h264 out.mp4` is reported by file(1) as "ISO Media, Apple
// QuickTime movie". file(1) reaches that verdict from exactly these bytes — the `ftyp` box at offset 4 with
// the `qt  ` major brand at offset 8 (QuickTime File Format specification, the File Type Compatibility
// atom). http.DetectContentType CANNOT see it: the WHATWG mp4 signature matches only a compatible brand
// beginning "mp4" (https://mimesniff.spec.whatwg.org/#signature-for-mp4), so a QuickTime container sniffs as
// application/octet-stream and an implementation that trusted the stdlib alone would publish the model's
// ".mp4" lie.
var quickTimeBytes = append([]byte{
	0x00, 0x00, 0x00, 0x14, 'f', 't', 'y', 'p', 'q', 't', ' ', ' ',
	0x00, 0x00, 0x02, 0x00, 'q', 't', ' ', ' ',
}, make([]byte, 64)...)

// recordedCall is one HTTP round trip the fake Slack peer saw.
type recordedCall struct {
	method, url, auth, contentType string
	body                           []byte
}

// uploadPeer is a fake Slack built from the PUBLISHED contract of the three-step external upload, not from
// the client under test — a fake shaped by its own client confirms itself.
//
// CONTRACT (both checked 2026-07-28):
//   - https://docs.slack.dev/reference/methods/files.getUploadURLExternal/ — success is
//     {"ok":true,"upload_url":"https://files.slack.com/upload/v1/…","file_id":"F123ABC456"}.
//   - https://docs.slack.dev/reference/methods/files.completeUploadExternal/ — success is
//     {"ok":true,"files":[{"id":"F123ABC456","title":"slack-test"}]}.
//   - https://docs.slack.dev/messaging/working-with-files/ — the middle POST is the raw bytes, and its
//     answer is the plain-text body "OK - <bytes>" rather than an API envelope.
type uploadPeer struct {
	calls     []recordedCall
	uploadURL string
	status    map[string]int    // per-path HTTP status override
	envelope  map[string]string // per-path body override
}

func newUploadPeer() *uploadPeer {
	return &uploadPeer{
		uploadURL: "https://files.slack.com/upload/v1/ABC123",
		status:    map[string]int{},
		envelope:  map[string]string{},
	}
}

func (p *uploadPeer) Do(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	p.calls = append(p.calls, recordedCall{
		method: r.Method, url: r.URL.String(), auth: r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"), body: body,
	})
	path := r.URL.Path
	status := http.StatusOK
	if s, ok := p.status[path]; ok {
		status = s
	}
	out := ""
	switch {
	case p.envelope[path] != "":
		out = p.envelope[path]
	case strings.HasSuffix(path, "/files.getUploadURLExternal"):
		out = `{"ok":true,"upload_url":"` + p.uploadURL + `","file_id":"F123ABC456"}`
	case strings.HasSuffix(path, "/files.completeUploadExternal"):
		out = `{"ok":true,"files":[{"id":"F123ABC456"}]}`
	default:
		out = "OK - " + strings.TrimSpace(string(rune('0'+len(body)%10)))
	}
	resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(out)), Header: http.Header{}}
	if status == http.StatusTooManyRequests {
		resp.Header.Set("Retry-After", "0")
	}
	return resp, nil
}

// modelText is the label a MODEL wrote. It is deliberately ugly: every assertion below hunts for it, and the
// point is that exactly one field may carry it.
const modelText = "MODEL-WROTE-THIS <!channel>"

func uploadFixture() Upload {
	return Upload{
		ChannelID: "C90", ThreadTS: "1700000090.000100",
		Filename: "art_0123456789abcdef0123456789abcdef.png",
		AltText:  "a PNG image produced by this run",
		Comment:  modelText,
		Body:     pngBytes,
	}
}

// THE THREE STEPS, IN THE DOCUMENTED ORDER, each addressed where the reference says it lives — and the
// middle one addressed at the URL STEP ONE RETURNED rather than at a URL we composed.
func TestUploadToThreadFollowsTheThreeDocumentedSteps(t *testing.T) {
	peer := newUploadPeer()
	if err := UploadToThread(context.Background(), peer, "https://slack.test/api", []byte("xoxb-SECRET"), uploadFixture()); err != nil {
		t.Fatalf("UploadToThread: %v", err)
	}
	if len(peer.calls) != 3 {
		t.Fatalf("the upload made %d call(s), want exactly 3: %+v", len(peer.calls), peer.calls)
	}
	for i, want := range []string{
		"https://slack.test/api/files.getUploadURLExternal",
		peer.uploadURL,
		"https://slack.test/api/files.completeUploadExternal",
	} {
		if peer.calls[i].url != want {
			t.Fatalf("step %d addressed %q, want %q", i+1, peer.calls[i].url, want)
		}
		if peer.calls[i].method != http.MethodPost {
			t.Fatalf("step %d used %s, want POST — all three steps are documented POSTs", i+1, peer.calls[i].method)
		}
	}

	// Step 1 declares the LENGTH of the bytes, which is why an artifact must be sized before it is uploaded
	// (plan §3.5 X10 — there is no streaming upload).
	var step1 map[string]any
	if err := json.Unmarshal(peer.calls[0].body, &step1); err != nil {
		t.Fatalf("step 1 body is not JSON: %v (%s)", err, peer.calls[0].body)
	}
	if step1["filename"] != "art_0123456789abcdef0123456789abcdef.png" {
		t.Fatalf("step 1 filename = %v, want the caller's content-derived name", step1["filename"])
	}
	if length, _ := step1["length"].(float64); int(length) != len(pngBytes) {
		t.Fatalf("step 1 declared length %v, want %d — the byte count, not an estimate", step1["length"], len(pngBytes))
	}

	// Step 2 is the RAW BYTES and carries NO CREDENTIAL: the upload URL is already the authorization.
	if got := peer.calls[1].body; string(got) != string(pngBytes) {
		t.Fatalf("step 2 sent %d byte(s), want the artifact's %d verbatim", len(got), len(pngBytes))
	}
	if peer.calls[1].auth != "" {
		t.Fatalf("step 2 carried an Authorization header (%q); the upload URL is the authorization and a bot token has no business on a third-party-visible URL", peer.calls[1].auth)
	}

	// Step 3 names the file id step 1 minted, ONE channel, and the thread's PARENT ts.
	var step3 struct {
		Files          []map[string]any `json:"files"`
		ChannelID      string           `json:"channel_id"`
		Channels       string           `json:"channels"`
		ThreadTS       string           `json:"thread_ts"`
		InitialComment string           `json:"initial_comment"`
		Blocks         any              `json:"blocks"`
	}
	if err := json.Unmarshal(peer.calls[2].body, &step3); err != nil {
		t.Fatalf("step 3 body is not JSON: %v (%s)", err, peer.calls[2].body)
	}
	if len(step3.Files) != 1 || step3.Files[0]["id"] != "F123ABC456" {
		t.Fatalf("step 3 files = %v, want the one file id step 1 returned", step3.Files)
	}
	if step3.ChannelID != "C90" || step3.Channels != "" {
		t.Fatalf("step 3 addressed channel_id=%q channels=%q; the reference says to \"provide only one channel when using thread_ts\"", step3.ChannelID, step3.Channels)
	}
	if step3.ThreadTS != "1700000090.000100" {
		t.Fatalf("step 3 thread_ts = %q, want the PARENT ts frozen on the delivery row", step3.ThreadTS)
	}
	if step3.Blocks != nil {
		t.Fatalf("step 3 sent `blocks` (%v). No published page says this method's blocks array accepts a markdown block, and a vendor's silence is not a design freedom (plan §3.5 X23) — the answer travels in initial_comment", step3.Blocks)
	}
	// The token rides the Authorization header on both API calls and nowhere else.
	for _, i := range []int{0, 2} {
		if peer.calls[i].auth != "Bearer xoxb-SECRET" {
			t.Fatalf("step %d auth = %q, want the bearer header", i+1, peer.calls[i].auth)
		}
		if strings.Contains(peer.calls[i].url, "xoxb-") || strings.Contains(string(peer.calls[i].body), "xoxb-") {
			t.Fatalf("step %d put the bot token in the URL or the body: %q / %s", i+1, peer.calls[i].url, peer.calls[i].body)
		}
	}
}

// EXACTLY ONE FIELD MAY CARRY THE MODEL'S WORDS, and the sweep JSON-DECODES before it looks: encoding/json
// escapes `<` into `<`, so a raw-substring assertion over a marshalled body can never fail (E20 T4 paid
// for that lesson in full).
func TestUploadCarriesTheModelsTextOnlyInInitialComment(t *testing.T) {
	peer := newUploadPeer()
	if err := UploadToThread(context.Background(), peer, "https://slack.test/api", []byte("xoxb-SECRET"), uploadFixture()); err != nil {
		t.Fatalf("UploadToThread: %v", err)
	}
	carriers := map[string]int{}
	for i, call := range peer.calls {
		var decoded any
		if err := json.Unmarshal(call.body, &decoded); err != nil {
			// Step 2 is raw bytes, not JSON. It must not contain the model's words either.
			if strings.Contains(string(call.body), "MODEL-WROTE-THIS") {
				t.Fatalf("step %d's raw body carries the model's text", i+1)
			}
			continue
		}
		walkJSONStrings(decoded, "", func(path, value string) {
			if strings.Contains(value, "MODEL-WROTE-THIS") {
				carriers[path]++
			}
		})
	}
	if len(carriers) != 1 || carriers["initial_comment"] == 0 {
		t.Fatalf("the model's text reached %v; exactly one field — initial_comment — may carry it", carriers)
	}
	// The sweep must DISCRIMINATE: a decoder that found nothing anywhere would have "passed" above with an
	// empty map, so the one legitimate carrier is asserted to have actually been found.
	if carriers["initial_comment"] != 1 {
		t.Fatalf("initial_comment carried the model's text %d time(s), want 1", carriers["initial_comment"])
	}
}

// walkJSONStrings visits every string in a decoded JSON document, naming it by the key it hangs off (array
// indices are transparent, so files[0].title is reported as "title").
func walkJSONStrings(node any, key string, visit func(path, value string)) {
	switch v := node.(type) {
	case string:
		visit(key, v)
	case map[string]any:
		for k, child := range v {
			walkJSONStrings(child, k, visit)
		}
	case []any:
		for _, child := range v {
			walkJSONStrings(child, key, visit)
		}
	}
}

// THE EXTENSION IS A FACT ABOUT THE BYTES. A model that calls its screen recording "demo.mp4" is describing
// a QuickTime container, and honouring the name would publish a lie into a workspace.
func TestSniffUploadDerivesTheExtensionFromContentNotFromAName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    []byte
		wantExt string
		wantMT  string
	}{
		{"a screenshot", pngBytes, ".png", "image/png"},
		{"a simctl recording", quickTimeBytes, ".mov", "video/quicktime"},
		{"a build log", []byte("** BUILD SUCCEEDED **\nCompiling Palai.swift\n"), ".txt", "text/plain"},
	} {
		got, ok := SniffUpload(tc.body)
		if !ok {
			t.Fatalf("%s: SniffUpload refused bytes it should publish", tc.name)
		}
		if got.Extension != tc.wantExt || got.MediaType != tc.wantMT {
			t.Fatalf("%s: sniffed %+v, want %s/%s", tc.name, got, tc.wantExt, tc.wantMT)
		}
		if got.AltText == "" {
			t.Fatalf("%s: no alt text; a screen reader gets nothing", tc.name)
		}
		if strings.Contains(got.AltText, "MODEL") {
			t.Fatalf("%s: the alt text came from somewhere it should not have: %q", tc.name, got.AltText)
		}
	}
	// Bytes we have no honest name for are REFUSED rather than published as ".bin".
	if got, ok := SniffUpload([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatalf("SniffUpload published an executable as %+v", got)
	}
	if _, ok := SniffUpload(nil); ok {
		t.Fatal("SniffUpload published empty bytes")
	}
}

// THE CEILING IS OURS. Nothing is uploaded past it and nothing is truncated into a corrupt file — the caller
// gets a typed refusal it can turn into an honest sentence.
func TestUploadRefusesAnArtifactOverOurOwnCeiling(t *testing.T) {
	peer := newUploadPeer()
	up := uploadFixture()
	up.Body = make([]byte, MaxUploadBytes+1)
	err := UploadToThread(context.Background(), peer, "https://slack.test/api", []byte("xoxb-SECRET"), up)
	if !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("an over-ceiling upload returned %v, want ErrUploadTooLarge", err)
	}
	if len(peer.calls) != 0 {
		t.Fatalf("an over-ceiling upload still made %d call(s); the refusal must cost nothing", len(peer.calls))
	}
}

// NO SECOND RETRY LAYER. ratelimit.go owns Slack's 429 for the sending path; a retry here would stack on it
// and turn one refused upload into a burst.
func TestUploadDoesNotRetryARateLimitedStep(t *testing.T) {
	peer := newUploadPeer()
	peer.status["/api/files.getUploadURLExternal"] = http.StatusTooManyRequests
	err := UploadToThread(context.Background(), peer, "https://slack.test/api", []byte("xoxb-SECRET"), uploadFixture())
	if err == nil {
		t.Fatal("a rate-limited upload reported success")
	}
	if len(peer.calls) != 1 {
		t.Fatalf("a 429 produced %d call(s), want exactly 1 — this file opens no retry layer", len(peer.calls))
	}
}

// A DOCUMENTED REFUSAL IS TYPED, so a caller can tell `missing_scope` (files:write was never granted) from a
// transport failure without matching on substrings.
func TestUploadSurfacesADocumentedAPIRefusal(t *testing.T) {
	peer := newUploadPeer()
	peer.envelope["/api/files.getUploadURLExternal"] = `{"ok":false,"error":"missing_scope"}`
	err := UploadToThread(context.Background(), peer, "https://slack.test/api", []byte("xoxb-SECRET"), uploadFixture())
	if code := APIErrorCode(err); code != "missing_scope" {
		t.Fatalf("a refused upload gave code %q (err %v), want missing_scope", code, err)
	}
	if len(peer.calls) != 1 {
		t.Fatalf("a refusal on step 1 still made %d call(s); the later steps must not run", len(peer.calls))
	}

	// And a refusal on the LAST step is surfaced too — the bytes are on Slack's side by then, but a file
	// nobody was shown is not a delivery.
	peer = newUploadPeer()
	peer.envelope["/api/files.completeUploadExternal"] = `{"ok":false,"error":"invalid_thread_ts"}`
	if code := APIErrorCode(UploadToThread(context.Background(), peer, "https://slack.test/api",
		[]byte("xoxb-SECRET"), uploadFixture())); code != CodeInvalidThreadTS {
		t.Fatalf("a refused completion gave code %q, want %s", code, CodeInvalidThreadTS)
	}
}
