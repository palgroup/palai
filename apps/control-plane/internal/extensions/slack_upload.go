package extensions

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// THE ARTIFACT UPLOAD LEG (E22 T5, spec §36) — the half of the owner's flagship request that makes the work
// VISIBLE. A run that codes on a Mac produces a screenshot, a screen recording and a build log; before this,
// a `file_ref` in its answer rendered as a link to bytes nobody in the thread could reach.
//
// WHAT CHANGES IS DELIVERY, NOT RENDER (plan §3.5 X13). The file_ref block is byte-identical to what it was:
// same link, same validation, same fallback. What is new is that the bytes now also ARRIVE.
//
// THE MODEL'S URL IS A NAME, NOT A FETCH TARGET, and that is the security argument in one line. Nothing here
// dials anything the model wrote. The reference is scanned for an artifact ID, and that id is then resolved
// against OUR OWN object store under THREE keys the model does not hold:
//
//  1. THE TENANT, from the delivery row (a foreign tenant's id is a miss, never an error — §22.6).
//  2. THE RUN, also from the delivery row. Tenant scoping alone is not enough: it would let this run publish
//     an artifact from somebody else's conversation in the same project, which is a screenshot leaving the
//     thread it was taken for.
//  3. THE SIZE, checked on the row before a byte is read.
//
// So the worst a model can do by writing a URL is name something that does not resolve, and the worst that
// does is nothing.
//
// THE ANSWER IS NEVER AT RISK (SLK-006). The uploads happen AFTER the reply is posted and the delivery row is
// marked delivered, so an upload that fails, a workspace that never granted files:write, and a Slack that is
// down all leave the run's own answer exactly where it landed. That ordering also makes the upload
// exactly-once for free: a delivered row is never re-claimed, which is what the vendor's "This method can
// only be called once" needs.
//
// THE HONEST CEILINGS, said here because a reader meets them here:
//
//   - A FILE IS NOT A VIDEO BLOCK (X12, structural). A Slack-hosted file cannot be a video block's source —
//     that needs a public, iframe-embeddable URL on one of the app's unfurl domains — so a screen recording
//     arrives as a file a human clicks, not as an inline player.
//   - THE UPLOAD IS AT RUN TERMINAL, not during it. A long iOS build cannot be WATCHED; its recording shows
//     up when the run ends.

// slackMaxUploadsPerReply caps how many artifacts one answer publishes. It is the outbound sibling of
// slackMaxImagesPerMessage and it exists for the same reason: the count is model-influenced, and three files
// is a thread somebody reads while thirty is a thread somebody leaves. It also bounds the memory this leg
// holds, since an artifact must be sized-and-held before it can be uploaded at all (plan §3.5 X10).
const slackMaxUploadsPerReply = 3

// artifactIDPrefix and artifactIDHexLen are the shape of an artifact id everywhere in this tree ("art_" plus
// 16 bytes of hex — newArtifactID and slackImageArtifactID both mint exactly that). Matching the SHAPE is
// what lets a reference be recognised inside a URL without parsing one: the id is a token that cannot be
// confused with a host, a path segment of the retrieval route, or a query value.
const (
	artifactIDPrefix = "art_"
	artifactIDHexLen = 32
)

// RunArtifactStore is the read half of the E09 artifact seam this leg publishes through, declared HERE as the
// consumer. *artifacts.Writer satisfies it; a fake in the component fixture serves it from memory.
//
// maxBytes is passed rather than fixed so the ceiling stays where it is decided — slack.MaxUploadBytes, with
// its reasoning — instead of being duplicated inside the store.
//
// THE OVER-CEILING ANSWER IS (found=true, size>maxBytes, NO BYTES) rather than a typed error, and that shape
// is forced rather than chosen: artifacts.Writer is the implementation, internal/execution imports THIS
// package, and this package importing artifacts back would close a cycle. So the contract is carried by the
// return values, and it is stated on both sides (see artifacts.Writer.ReadRunArtifact).
type RunArtifactStore interface {
	ReadRunArtifact(ctx context.Context, project, runID, artifactID string, maxBytes int64) ([]byte, int64, bool, error)
}

// WithArtifactUpload mounts the upload leg. Without it a run's answer is posted exactly as it was before and
// a file_ref stays a link — the same nil-safe posture every optional half of this bridge takes.
func (a *SlackAdmitter) WithArtifactUpload(store RunArtifactStore) *SlackAdmitter {
	a.runArtifacts = store
	return a
}

// ArtifactUploadReady reports whether an artifact a run produced can reach the thread as a file at all. It
// exists for the reason FileFetchReady does: this leg was built once already in the inbound direction and
// mounted behind a nil check in a deployment that had no object store, and the only evidence was a
// parenthetical inside a prompt. The composition root logs this at boot.
func (a *SlackAdmitter) ArtifactUploadReady() bool { return a.runArtifacts != nil }

// pendingUpload is one artifact resolved and ready to publish. The bytes are held because there is no
// streaming upload (X10) and the length must be declared before the transfer.
type pendingUpload struct {
	artifactID string
	comment    string
	sniffed    slack.SniffedUpload
	body       []byte
}

// resolveUploads turns the file_refs in a run's answer into artifacts ready to publish, and returns the
// sentence the ANSWER must carry for anything refused.
//
// The note is built here, before the answer is posted, precisely so a refusal is visible in the message a
// human reads rather than only in a log an operator might. "Not uploaded" and "not mentioned" together are a
// silent drop, and the plan names that as the one thing this must never be.
func (p *SlackReplyPump) resolveUploads(ctx context.Context, o slackReplyOrder, answer string) ([]pendingUpload, string) {
	a := p.bridge
	if a.runArtifacts == nil {
		// Not mounted. NOT logged per reply — a permanent configuration fact belongs in one boot line, not
		// under traffic (FileFetchReady's comment has the full story).
		return nil, ""
	}
	refs := artifactRefs(answer, slackMaxUploadsPerReply)
	if len(refs) == 0 {
		return nil, ""
	}
	var ready []pendingUpload
	oversize, unreadable := 0, 0
	for _, ref := range refs {
		body, size, found, err := a.runArtifacts.ReadRunArtifact(ctx, o.project, o.runID, ref.id, slack.MaxUploadBytes)
		switch {
		case err != nil:
			log.Printf("slack: could not read artifact %s for run %s: %v", ref.id, o.runID, err)
			unreadable++
			continue
		case !found:
			// Unknown id, another tenant's, another run's, or bytes retention already reclaimed. All one
			// answer, deliberately: distinguishing them for the reader would be an existence oracle.
			unreadable++
			continue
		case size > slack.MaxUploadBytes:
			log.Printf("slack: artifact %s (%d bytes) is over the %d-byte upload ceiling; run %s's answer says so and links to it instead",
				ref.id, size, slack.MaxUploadBytes, o.runID)
			oversize++
			continue
		}
		sniffed, ok := slack.SniffUpload(body)
		if !ok {
			log.Printf("slack: not publishing artifact %s for run %s: its bytes are not a type this integration names honestly", ref.id, o.runID)
			unreadable++
			continue
		}
		ready = append(ready, pendingUpload{
			artifactID: ref.id,
			// THE ONLY MODEL TEXT THAT REACHES SLACK ON THIS PATH, and it reaches exactly one field
			// (slack.Upload.Comment -> initial_comment). Defused with the SAME function the block and text
			// paths use rather than a second copy of the rule.
			comment: slack.NeutralizeBroadcasts(ref.label),
			sniffed: sniffed,
			body:    body,
		})
	}
	return ready, uploadNote(oversize, unreadable)
}

// uploadNote is the visible half of every refusal this leg makes. It says the COUNT and the reason class and
// nothing else: which artifact, and why exactly, is operator detail, and an artifact id in a channel is noise
// to everyone who reads it.
//
// It TRAILS the answer for the same reason slackImageNote trails the human's words: it is entirely ours — a
// fixed sentence and an integer, with no field of any payload in it — so it is not untrusted annotation that
// has to lead.
func uploadNote(oversize, unreadable int) string {
	var parts []string
	switch {
	case oversize == 1:
		parts = append(parts, fmt.Sprintf("one file this run produced is over the %d MiB upload limit, so it is linked above rather than attached", slack.MaxUploadBytes>>20))
	case oversize > 1:
		parts = append(parts, fmt.Sprintf("%d files this run produced are over the %d MiB upload limit, so they are linked above rather than attached", oversize, slack.MaxUploadBytes>>20))
	}
	switch {
	case unreadable == 1:
		parts = append(parts, "one file this run referred to could not be attached")
	case unreadable > 1:
		parts = append(parts, fmt.Sprintf("%d files this run referred to could not be attached", unreadable))
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n\n(" + strings.Join(parts, "; ") + ")"
}

// publishUploads posts each resolved artifact into the thread. It is called AFTER the answer has landed and
// the delivery row is settled, so nothing it does can cost the run its answer.
//
// It paces per channel between files for the same reason the reply does: the documented Special Tier is a
// per-channel budget, and three files in one instant is exactly the burst it exists to shed.
func (p *SlackReplyPump) publishUploads(ctx context.Context, o slackReplyOrder, token []byte, uploads []pendingUpload) {
	a := p.bridge
	for _, up := range uploads {
		if a.pacer != nil {
			if err := a.pacer.Wait(ctx, o.channelID); err != nil {
				return // cancelled; the answer is already delivered and nothing is owed
			}
		}
		err := slack.UploadToThread(ctx, a.doer, a.apiBase, token, slack.Upload{
			ChannelID: o.channelID,
			// THE PARENT, and it needs no new column: slack_reply_deliveries froze the thread's root ts at
			// enqueue, which is what the vendor demands ("Never use a reply's `ts` value; use its parent
			// instead"). Verified against 000036's own comment before anything was added.
			ThreadTS: o.threadTS,
			// OURS, from the bytes. The model's name for the file never travels: `--codec=h264` still writes a
			// QuickTime container, so honouring a model's ".mp4" would publish a lie (plan §3.5 X2).
			Filename: up.artifactID + up.sniffed.Extension,
			AltText:  up.sniffed.AltText,
			Comment:  up.comment,
			Body:     up.body,
		})
		if err != nil {
			// SLK-006: the answer is already in the thread and the run is untouched. This is logged and
			// abandoned rather than retried — the delivery row is settled, and re-claiming it to retry a file
			// would risk a second copy of the ANSWER to save one attachment.
			log.Printf("slack: run %s's answer landed but artifact %s did not reach the thread: %v", o.runID, up.artifactID, err)
		}
	}
}

// artifactRef is one file_ref the model wrote, reduced to the two things that matter: the artifact it NAMES
// and the words it labelled it with.
type artifactRef struct{ id, label string }

// artifactRefs pulls the artifact ids a run's answer names through the file_ref variant, in the order the
// model wrote them, capped.
//
// IT PARSES THE ANSWER WITH THE RENDERER'S OWN PARSER (slack.ParseResults), which is what keeps the upload
// and the render talking about the same thing: a variant the renderer treats as inert text is not a file_ref
// here either, and a shape that never becomes a Result can never become an upload.
func artifactRefs(answer string, max int) []artifactRef {
	var out []artifactRef
	seen := map[string]bool{}
	for _, result := range slack.ParseResults(answer) {
		if result.Type != slack.ResultFileRef {
			continue
		}
		id := artifactIDFromRef(result.URL)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, artifactRef{id: id, label: result.Label})
		if len(out) >= max {
			break
		}
	}
	return out
}

// artifactIDFromRef finds an artifact id inside a reference a model wrote. The reference is expected to be
// the artifact's own retrieval URL (/v1/artifacts/{id}/content, the route this tree already serves), but a
// bare id is accepted too — a model that names the thing directly should not be punished for it.
//
// IT IS A SCAN FOR A SHAPE, NOT A URL PARSE, and that is the conservative choice rather than the lazy one: no
// part of this decides that a URL is trustworthy, because no part of this DIALS it. The string is searched
// for a token that looks exactly like an artifact id, and whether that id means anything is decided by a
// tenant-and-run-scoped read against our own store. An id that names nothing simply resolves to nothing.
//
// The FIRST match wins and it must be a whole token: an id embedded in a longer hex run is not a match, so
// "art_" + 40 hex characters names nothing rather than naming its own prefix.
func artifactIDFromRef(ref string) string {
	for i := 0; i+len(artifactIDPrefix)+artifactIDHexLen <= len(ref); i++ {
		if !strings.HasPrefix(ref[i:], artifactIDPrefix) {
			continue
		}
		if i > 0 && isIDByte(ref[i-1]) {
			continue // it is the tail of a longer token, not an id
		}
		start := i + len(artifactIDPrefix)
		end := start + artifactIDHexLen
		if !allHex(ref[start:end]) {
			continue
		}
		if end < len(ref) && isIDByte(ref[end]) {
			continue // longer than an id, so it is not one
		}
		return ref[i:end]
	}
	return ""
}

func isIDByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func allHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
