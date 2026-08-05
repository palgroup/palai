package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The FILE FETCH (E20, spec §36) — the half SLK-005 classified and then deferred. Its case text says a file
// share is "classified for a scoped fetch" and that "the file leg's scoped fetch + scan + artifact +
// token-discard reuses the existing E09 artifact/scan seam"; this is the fetch, and the artifact half lives
// control-plane-side where the object store is (spec §24).
//
// WHAT MAKES THIS FETCH SCOPED, in order, because "scoped" is the whole security argument and every clause
// is a different attack:
//
//  1. THE MESSAGE WAS ADMITTED FIRST. The caller only reaches here after the signature verified, the
//     connection resolved, allowed_channels passed and the run-birth rule said yes. A file id that merely
//     APPEARS in a payload never becomes a fetch — the confused-deputy shape E20 T3 refused for context
//     entities, and it applies harder here because this app holds `files:read`: a fetch driven by an
//     attacker-chosen id is a read primitive over the whole workspace.
//  2. THE HOST IS FIXED. The token is presented ONLY to Slack's own file hosts over TLS (see trustedFileURL).
//     The URL comes out of a signature-verified payload, so today it is Slack's — the guard is what makes
//     that a property of this code rather than of Slack never renaming a field.
//  3. THE BYTES DECIDE WHAT THEY ARE. The media type is sniffed; a declared mimetype and a filename are the
//     uploader's words, so they are data, never authority.
//  4. THE SIZE IS BOUNDED TWICE — the declared size before the transfer, the actual body during it.
//  5. THE TOKEN LEAVES NOTHING BEHIND. It rides one Authorization header, and no error, log, artifact row or
//     provenance map built from this path carries it.
//
// NOT SOLVED HERE, and said plainly where a reader meets it: text rendered INSIDE an image reaches the model
// as content it can read, so an image is a prompt-injection surface exactly as a message is. Nothing in this
// file changes that; what it guarantees is that an image cannot change the run's AUTHORITY (see
// execution.decodeContentParts).

// slackFileHosts are the hosts a private file may be fetched from. Slack serves file content from
// files.slack.com (https://docs.slack.dev/messaging/working-with-files/, checked 2026-07-27, which documents
// url_private / url_private_download on that host). files-edge.slack.com appears on some workspaces' file
// objects; it is included because it is the same first-party origin and excluding it would silently drop
// those workspaces' files.
var slackFileHosts = map[string]bool{
	"files.slack.com":      true,
	"files-edge.slack.com": true,
}

var (
	// ErrUntrustedFileHost refuses to present the bot token to anything but a Slack file host.
	ErrUntrustedFileHost = errors.New("slack: refusing to fetch a file from a non-Slack host")
	// ErrFileTooLarge refuses a file over the caller's ceiling — declared or actual.
	ErrFileTooLarge = errors.New("slack: the shared file is over the size ceiling")
	// ErrNotAnImage refuses bytes that are not an image whatever the uploader called them.
	ErrNotAnImage = errors.New("slack: the shared file's bytes are not an image")
)

// SharedFile is the subset of a Slack file object this integration reads. Every field except ID is the
// UPLOADER's claim about the file and is treated as such: MimeType and Name are used only to decide whether
// a file is worth fetching at all and never to label the bytes, and Size only to refuse early.
type SharedFile struct {
	ID          string
	Name        string
	MimeType    string
	Size        int64
	DownloadURL string
}

// FetchedImage is one fetched image: its bytes and the media type they were SNIFFED as.
type FetchedImage struct {
	MediaType string
	Content   []byte
}

// imageMediaTypes are the media types a fetched image may be, after sniffing. It is the intersection of what
// Slack hosts and what an OpenAI-compatible vision endpoint accepts
// (https://developers.openai.com/api/docs/guides/vision, checked 2026-07-27: PNG, JPEG, WEBP and
// non-animated GIF). A type outside it is refused rather than sent, because a provider rejecting it mid-run
// is a worse failure than refusing it at the door.
var imageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// ImageCandidates picks the files worth fetching and reports how many were left out.
//
// It is a PURE FUNCTION of the event and its answer is ORDER-STABLE, which is load bearing rather than
// tidy: the run input built from it is hashed into the admission's idempotency record, so a redelivery that
// re-derives a different set would turn SLK-002's replay into a conflict.
//
// THE COUNT CAP IS APPLIED LAST, after every disqualification, and the ordering was a real bug caught by
// TestSlackNonImageAndOversizeFilesAreRefusedVisibly: capping first let a file that was never going to be
// fetched — a 50 MiB "image" — consume one of the slots, so a message with one huge file and three
// screenshots delivered two screenshots instead of three. A cap should bound what is ATTEMPTED.
//
// maxBytes disqualifies a DECLARED oversize here as well as in FetchImage. That is not redundancy for its own
// sake: this is the cheap filter that keeps the slot, and that one is the bound a direct caller cannot skip.
//
// The skipped COUNT is returned instead of the skipped files because the caller says so in the prompt, and
// a filename is untrusted text that has no business being quoted into a conversation.
func ImageCandidates(files []SharedFile, max int, maxBytes int64) (kept []SharedFile, skipped int) {
	for _, f := range files {
		switch {
		case f.DownloadURL == "":
			skipped++ // nothing to fetch: a file object with no private download url
		case !strings.HasPrefix(strings.ToLower(f.MimeType), "image/"):
			skipped++ // not offered as an image; the bytes are never fetched to find out
		case f.Size > maxBytes:
			skipped++ // declares more than the ceiling: refused without paying for the transfer
		case len(kept) >= max:
			skipped++
		default:
			kept = append(kept, f)
		}
	}
	return kept, skipped
}

// FetchImage downloads one shared file with the bot token and returns its bytes plus their sniffed media
// type. maxBytes bounds the transfer.
//
// token is used for exactly one thing — the Authorization header of this request — and is never placed in
// the URL, an error, or the returned value. A URL-borne credential would land in every proxy and access log
// between here and Slack, which is why the host check rejects a plaintext http:// URL too.
func FetchImage(ctx context.Context, doer Doer, token []byte, file SharedFile, maxBytes int64) (FetchedImage, error) {
	if err := trustedFileURL(file.DownloadURL); err != nil {
		return FetchedImage{}, err
	}
	// The declared size is the uploader's word, so it cannot AUTHORIZE a transfer — but it can refuse one,
	// and refusing before the request means not paying for the bytes at all.
	if file.Size > maxBytes {
		return FetchedImage{}, fmt.Errorf("%w: file %s declares %d bytes, ceiling is %d", ErrFileTooLarge, file.ID, file.Size, maxBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.DownloadURL, nil)
	if err != nil {
		return FetchedImage{}, fmt.Errorf("slack: build file request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(token)) // the sole use of the credential
	resp, err := doer.Do(req)
	if err != nil {
		// Not wrapped with %w on purpose beyond the transport error: a redirect error from net/http can
		// echo the request URL, and that is the only place a URL could carry anything sensitive.
		return FetchedImage{}, fmt.Errorf("slack: fetch file %s: %w", file.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body is NOT echoed: a Slack error page is untrusted bytes, and the status is the whole signal
		// an operator needs.
		return FetchedImage{}, fmt.Errorf("slack: fetch file %s: HTTP %d", file.ID, resp.StatusCode)
	}

	// Read one byte past the ceiling so an over-size body is DETECTED rather than silently truncated into a
	// corrupt image. Content-Length is not consulted: it is absent on a chunked response and is in any case
	// a claim, while this is a measurement.
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return FetchedImage{}, fmt.Errorf("slack: read file %s: %w", file.ID, err)
	}
	if int64(len(content)) > maxBytes {
		return FetchedImage{}, fmt.Errorf("%w: file %s exceeds %d bytes on the wire", ErrFileTooLarge, file.ID, maxBytes)
	}

	// SNIFFED, not declared. http.DetectContentType implements the WHATWG sniffing algorithm over the first
	// 512 bytes and recognises exactly the image families above by magic number, so a shell script named
	// "screenshot.png" and served as image/png is refused here.
	mediaType := strings.TrimSpace(strings.Split(http.DetectContentType(content), ";")[0])
	if !imageMediaTypes[mediaType] {
		return FetchedImage{}, fmt.Errorf("%w: file %s sniffed as %s", ErrNotAnImage, file.ID, mediaType)
	}
	return FetchedImage{MediaType: mediaType, Content: content}, nil
}

// trustedFileURL admits only an https URL on a Slack file host, matched on the EXACT host — never a suffix,
// which is what "files.slack.com.evil.example.com" is for. An empty or unparsable URL is refused, and so is
// one carrying userinfo: a `user:pass@` prefix in a URL is a credential we did not put there.
func trustedFileURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || !slackFileHosts[u.Hostname()] {
		// The URL is deliberately not echoed: it is attacker-adjacent input, and an error string is a place
		// values get logged.
		return fmt.Errorf("%w: only https on a Slack file host is fetched", ErrUntrustedFileHost)
	}
	return nil
}

// ---------------------------------------------------------------------------------------------------------
// THE FILE UPLOAD (E22 T5, spec §36) — the other direction, and the half E20 T4 wrote down and deliberately
// did not build. Its words were exact: "FILE UPLOAD IS NOT BUILT (S14) … files:write is a new standing write
// access", and the reason given was that no scenario needed it yet. E22 IS that scenario: a run that codes on
// a Mac produces a screenshot, a screen recording and a build log, and a link to bytes nobody can reach is
// not a delivery.
//
// WHAT THIS CODE IS: three HTTP calls in a documented order. It holds no policy. WHICH artifact is uploaded,
// WHOSE it is and whether it may be published at all is a CALLER's decision, made where the tenant and the
// run are known. This file's whole job is to be exactly what the vendor documents, and to make three rules
// structural rather than remembered:
//
//  1. THE MODEL'S WORDS TRAVEL IN ONE FIELD. initial_comment, and nothing else. `blocks` is not sent at all
//     (see completeUpload), so there is exactly one place a caller has to neutralise.
//  2. THE FILENAME IS A FACT ABOUT THE BYTES. SniffUpload derives it; the caller cannot pass a name a model
//     chose, because the caller has no name to pass — it asks this file what the bytes are.
//  3. THE CEILING IS OURS AND IT IS CHECKED BEFORE THE FIRST CALL. An artifact over it costs zero requests.
//
// THIS LEG HAS NO CALLER TODAY, measured 2026-08-05 (`grep -rn 'slack\.\(SniffUpload\|UploadToThread\)' --
// include='*.go' . | grep -v /adapters/integrations/slack/ | grep -v _test.go` → empty) — NOT because the
// capability is obsolete, unlike the HTTP-transport functions removed alongside this measurement
// (ParseChallenge, VerifySignature and the rest). The caller that once drove this — the old in-process
// bridge's extensions/slack_upload.go (E22 T5), which scanned a run's file_ref blocks for artifact ids and
// uploaded the matching bytes so a screenshot or a screen recording a run produced actually arrived in the
// thread — was deleted whole with the rest of that bridge in the 2026-08-04 SDK-relay cutover, and no
// successor has been written for apps/slack-bot's own relay yet. Confirmed SEPARATELY blocked besides that:
// this deployment's Slack app is not granted `files:write` (files.getUploadURLExternal answers missing_scope
// with today's token). Both are real, and neither is a reason to delete working code that a wiring task and
// an OAuth reinstall are what stand between this and the same capability the old bridge had.

// MaxUploadBytes is the largest artifact this integration will publish into a thread. IT IS OUR NUMBER, NOT
// SLACK'S: neither files.getUploadURLExternal nor files.completeUploadExternal prints a maximum anywhere
// (both checked 2026-07-28), and plan §3.5 X23 records that absence as UNCONFIRMED rather than inventing a
// vendor limit to hide behind.
//
// 8 MiB is chosen against what this actually carries. A simulator screenshot is a few hundred KB; a build log
// is smaller; a short screen recording is a few MB. Past that the artifact is not a thing a human scrolls
// past in a channel — it is a download — and the honest answer is a sentence saying where it lives, not a
// silent drop and not a 300 MB transfer through a control plane holding it in memory.
//
// It is a CONSTANT rather than a knob because a per-deployment ceiling would make "why did my video not
// arrive" a question with a different answer in every workspace.
const MaxUploadBytes = 8 << 20

// ErrUploadTooLarge refuses an artifact over MaxUploadBytes. It is typed because the caller does not merely
// log it: it turns it into a sentence the human reads (plan §4 T5 — "never a silent drop").
var ErrUploadTooLarge = errors.New("slack: the artifact is over the upload ceiling")

// Upload is one artifact on its way to a thread.
//
// ThreadTS MUST BE THE PARENT. CONTRACT: https://docs.slack.dev/reference/methods/files.completeUploadExternal/
// (checked 2026-07-28) — "Never use a reply's `ts` value; use its parent instead." The value this integration
// passes is the one slack_reply_deliveries froze at enqueue, which IS the parent (it is the thread's root ts,
// copied from the event that birthed the run), so no new column was needed to hold it.
//
// Comment is the ONLY field that may carry text a model wrote, and the caller neutralises it before it
// arrives. Filename and AltText come from SniffUpload — i.e. from the bytes — and never from a model.
type Upload struct {
	ChannelID string
	ThreadTS  string
	Filename  string
	AltText   string
	Comment   string
	Body      []byte
}

// SniffedUpload is what the BYTES are: the extension they earn, the media type they sniffed as, and an alt
// text describing that type. Every field is derived here so that none of them can be a model's claim.
type SniffedUpload struct {
	Extension string
	MediaType string
	AltText   string
}

// uploadKinds is the closed set of things this integration publishes, keyed by SNIFFED media type. A type
// outside it is refused: an extension we cannot justify is a name that says something false about the bytes,
// and ".bin" in a Slack thread is worse than an honest refusal.
//
// The alt texts are OURS, fixed, and describe the CONTAINER rather than the content — this code has not seen
// the screenshot and cannot describe it, and inventing a description would be a lie with a screen reader as
// its audience.
var uploadKinds = map[string]SniffedUpload{
	"image/png":       {Extension: ".png", MediaType: "image/png", AltText: "a PNG image produced by this run"},
	"image/jpeg":      {Extension: ".jpg", MediaType: "image/jpeg", AltText: "a JPEG image produced by this run"},
	"video/quicktime": {Extension: ".mov", MediaType: "video/quicktime", AltText: "a QuickTime screen recording produced by this run"},
	"video/mp4":       {Extension: ".mp4", MediaType: "video/mp4", AltText: "an MP4 screen recording produced by this run"},
	"text/plain":      {Extension: ".txt", MediaType: "text/plain", AltText: "a plain-text log produced by this run"},
}

// SniffUpload names what bytes are, and returns false for anything this integration will not publish.
//
// WHY IT DOES NOT SIMPLY TRUST http.DetectContentType: it cannot see a QuickTime container. The WHATWG mp4
// signature (https://mimesniff.spec.whatwg.org/#signature-for-mp4, which net/http implements) matches only a
// brand beginning "mp4", so a file whose major brand is `qt  ` sniffs as application/octet-stream. And a
// QuickTime container is exactly what `xcrun simctl io recordVideo` writes — INCLUDING with `--codec=h264`,
// MEASURED on this machine 2026-07-28 (plan §3.5 X2): file(1) answers "ISO Media, Apple QuickTime movie".
// A model that names that recording "demo.mp4" is not lying on purpose; publishing its name would be.
//
// So the stdlib does the families it knows and this adds the one it structurally cannot.
func SniffUpload(body []byte) (SniffedUpload, bool) {
	if len(body) == 0 {
		return SniffedUpload{}, false
	}
	mediaType := "video/quicktime"
	if !isQuickTime(body) {
		mediaType = strings.TrimSpace(strings.Split(http.DetectContentType(body), ";")[0])
	}
	kind, ok := uploadKinds[mediaType]
	return kind, ok
}

// isQuickTime reports whether the bytes open with a File Type Compatibility atom whose MAJOR BRAND is `qt  `.
//
// CONTRACT: the QuickTime File Format specification's `ftyp` atom — a 4-byte big-endian size, the type
// `ftyp`, then the major brand — https://developer.apple.com/documentation/quicktime-file-format/ (checked
// 2026-07-28). Only the major brand is read; compatible brands further in the atom are not consulted,
// because a file that calls itself QuickTime FIRST is a QuickTime file whatever else it also claims to be.
func isQuickTime(body []byte) bool {
	return len(body) >= 12 && string(body[4:8]) == "ftyp" && string(body[8:12]) == "qt  "
}

// UploadToThread publishes one artifact into a thread as a real file, in the three documented steps. It
// returns a typed *APIError for a documented refusal (see APIErrorCode) and ErrUploadTooLarge for an artifact
// over our own ceiling.
//
// NO RETRY LAYER LIVES HERE. ratelimit.go owns Slack's 429 for the sending path and the caller paces per
// channel before calling; a retry here would stack on both, and a 429 answered with an immediate re-upload is
// how one refused file becomes a burst. A rate-limited upload is a failed upload, and the caller's answer is
// already safe (SLK-006).
func UploadToThread(ctx context.Context, doer Doer, apiBase string, token []byte, up Upload) error {
	if len(up.Body) == 0 {
		return errors.New("slack: refusing to upload an empty artifact")
	}
	// BEFORE the first call, so an over-size artifact costs nothing: not a request, not a transfer, and not a
	// half-uploaded file on Slack's side.
	if len(up.Body) > MaxUploadBytes {
		return fmt.Errorf("%w: %d bytes, ceiling is %d", ErrUploadTooLarge, len(up.Body), MaxUploadBytes)
	}

	uploadURL, fileID, err := getUploadURL(ctx, doer, apiBase, token, up)
	if err != nil {
		return err
	}
	if err := putUploadBytes(ctx, doer, uploadURL, up.Body); err != nil {
		return err
	}
	return completeUpload(ctx, doer, apiBase, token, fileID, up)
}

// getUploadURL is step one: reserve a one-shot upload URL for a file of a known name and length.
//
// CONTRACT: https://docs.slack.dev/reference/methods/files.getUploadURLExternal/ (checked 2026-07-28) —
// POST, `application/x-www-form-urlencoded` or `application/json`, scope `files:write`. REQUIRED: `filename`
// ("Name of the file being uploaded") and `length` ("Size in bytes of the file being uploaded"). OPTIONAL:
// `alt_txt` ("Description of image for screen-reader") and `snippet_type`. Success is
// {"ok":true,"upload_url":…,"file_id":…}; a refusal is {"ok":false,"error":…}.
//
// THE LENGTH IS REQUIRED UP FRONT, and that is a design constraint rather than a parameter: there is no
// streaming upload, so an artifact has to be sized — and therefore held — before this call (plan §3.5 X10).
// That is why the ceiling above is a memory decision as much as a taste one.
//
// alt_txt is sent from SniffUpload's fixed text, never from a model: it is a field that reaches a human
// through a screen reader, and it is not initial_comment.
func getUploadURL(ctx context.Context, doer Doer, apiBase string, token []byte, up Upload) (uploadURL, fileID string, err error) {
	body, err := json.Marshal(map[string]any{
		"filename": up.Filename,
		"length":   len(up.Body),
		"alt_txt":  up.AltText,
	})
	if err != nil {
		return "", "", fmt.Errorf("slack: build upload reservation: %w", err)
	}
	raw, err := postSlackJSON(ctx, doer, apiBase+"/files.getUploadURLExternal", token, body, "reserve an upload url")
	if err != nil {
		return "", "", err
	}
	var env struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", "", fmt.Errorf("slack: decode the upload reservation: %w", err)
	}
	if env.UploadURL == "" || env.FileID == "" {
		return "", "", errors.New("slack: the upload reservation carried no upload_url/file_id")
	}
	return env.UploadURL, env.FileID, nil
}

// putUploadBytes is step two: the bytes themselves, to the URL step one returned.
//
// CONTRACT: https://docs.slack.dev/messaging/working-with-files/ (checked 2026-07-28) — POST the raw file
// data (`Content-Type: application/octet-stream`, `--data-binary`), with NO token: "the upload URL from step
// one already contains the necessary authorization". The answer is a plain-text body ("OK - <bytes>"), not an
// API envelope — "Check the HTTP status code to verify the file's successful upload."
//
// NO Authorization HEADER IS SET, and that is the rule rather than an omission: this URL comes from a
// response body, and presenting the bot token to a URL we did not compose is how a credential leaves a
// system. The status code is the whole signal; the body is read only to drain the connection.
func putUploadBytes(ctx context.Context, doer Doer, uploadURL string, content []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("slack: build the upload: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := doer.Do(req)
	if err != nil {
		return fmt.Errorf("slack: upload the artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		// The body is NOT echoed: it is bytes from a URL that arrived in a response, and an error string is a
		// place values get logged.
		return fmt.Errorf("slack: the artifact upload answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// completeUpload is step three: share the uploaded file into ONE thread.
//
// CONTRACT: https://docs.slack.dev/reference/methods/files.completeUploadExternal/ (checked 2026-07-28) —
// POST, JSON accepted, scope `files:write`. REQUIRED `files` ("Array of file ids and their corresponding
// (optional) titles"). OPTIONAL `channel_id`, `thread_ts` ("Provide another message's `ts` value to upload
// this file as a reply"), `initial_comment` ("The message text introducing the file in specified channels"),
// `blocks`, `channels`, and the three chat:write.customize fields. Verbatim warnings, all three honoured
// here: "Never use a reply's `ts` value; use its parent instead", "make sure to provide only one channel when
// using `thread_ts`", and "This method can only be called once."
//
// WHAT IS DELIBERATELY NOT SENT:
//
//   - `blocks`. No published page states that this method's blocks array accepts a `markdown` block, and a
//     vendor's silence is not a design freedom (plan §3.5 X23). This is the same fail-closed reading E21 T6
//     held for chat.stopStream. The consequence is small and honest: the introducing text is plain.
//   - `channels`. The reference offers a comma-separated multi-channel form, which the thread warning above
//     forbids anyway; `channel_id` alone makes "only one channel" structural instead of remembered.
//   - a file `title`. It is optional, Slack shows the filename without it, and a title is one more field a
//     caller could put a model's words in.
//   - `username`/`icon_url`/`icon_emoji`. They need chat:write.customize, which is not requested — an app
//     that can repaint its own identity per message is a phishing surface.
//
// "CALLED ONCE" is not a retry problem here: the caller uploads only after the reply row is marked delivered,
// so a claimed-and-delivered row is never re-claimed and this method is never called twice for one artifact.
func completeUpload(ctx context.Context, doer Doer, apiBase string, token []byte, fileID string, up Upload) error {
	payload := map[string]any{
		"files":      []any{map[string]any{"id": fileID}},
		"channel_id": up.ChannelID,
		"thread_ts":  up.ThreadTS,
	}
	if up.Comment != "" {
		payload["initial_comment"] = up.Comment
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack: build the upload completion: %w", err)
	}
	_, err = postSlackJSON(ctx, doer, apiBase+"/files.completeUploadExternal", token, body, "complete the upload")
	return err
}

// postSlackJSON is the one round trip the two API steps share: a JSON POST with the bot token in the
// Authorization header, an envelope decoded to {ok,error}, and a typed *APIError for a documented refusal.
//
// It is NOT PostMessage: that function decodes a chat `ts` and owns the bounded 429 repair for the sending
// path, and neither fits here (these methods return different envelopes, and see UploadToThread on why no
// retry lives in this file).
func postSlackJSON(ctx context.Context, doer Doer, methodURL string, token []byte, body []byte, what string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, methodURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("slack: build the request to %s: %w", what, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if len(token) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(token)) // the sole use of the credential
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack: %s: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("slack: read the answer to %s: %w", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		// Includes 429; see UploadToThread. Not retried, and the body is not echoed.
		return nil, fmt.Errorf("slack: %s answered HTTP %d", what, resp.StatusCode)
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("slack: decode the answer to %s: %w", what, err)
	}
	if !env.OK {
		if env.Error == "" {
			env.Error = "unknown"
		}
		return nil, fmt.Errorf("slack: %s failed: %w", what, &APIError{Code: env.Error})
	}
	return raw, nil
}
