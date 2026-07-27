package slack

import (
	"context"
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
