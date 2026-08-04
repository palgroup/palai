package relay

import (
	"context"
	"fmt"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// THE IMAGE LEG — a screenshot somebody pasted at the bot, made visible to the model.
//
// It is the SDK-relay half of a capability the control plane already had on both ends and could not join
// from outside: `image_ref` has been a content item since the contract was written, and
// execution.decodeContentParts resolves one to bytes control-plane-side, but every writer of an artifact
// row lived INSIDE the control plane (internal/extensions' Slack bridge). This relay consumes Palai as a
// customer over `/v1` and nothing else, so the missing link was a public write verb — POST /v1/artifacts,
// added alongside this file. What this file does is the three steps in between: pick which shared files are
// worth fetching, fetch them with the bot token, and hand the bytes to Palai.
//
// EVERYTHING SECURITY-CRITICAL ABOUT THE FETCH ALREADY EXISTED and is reused rather than rewritten:
// slack.ImageCandidates disqualifies non-images and declared-oversize files, and slack.FetchImage pins the
// host to Slack's own file origins over TLS, bounds the transfer, sniffs the bytes rather than believing
// the uploader's mimetype, and keeps the token in exactly one header. That code was written for the old
// in-process path; none of its reasons changed when the caller moved out of process, so copying it would
// have meant maintaining two copies of an argument that is right once.
//
// WHAT THIS FILE DOES NOT DO, deliberately: it never decides whether a message SHOULD be answered. The
// caller has already made that decision (see HandleEvent) and this runs after it, which is the same
// ordering the old path called "the message was admitted first" — a file id that merely appears in some
// payload never becomes a fetch, because this app holds `files:read` and a fetch driven by an id somebody
// else chose is a read primitive over the whole workspace.

// maxImagesPerMessage caps how many images ONE Slack message contributes.
//
// Slack allows ten files per message, so this is a REFUSAL of some of them, and the refusal is SAID (see
// imageNote) rather than made silently — a model answering about three screenshots when a human attached
// five, with nothing anywhere saying so, is worse than a model told it cannot see two of them.
//
// It is deliberately below the control plane's own per-conversation ceiling (execution.maxRunImages, 8):
// that one counts across the WHOLE conversation, so leaving headroom means a second message in the same
// thread can still attach something instead of being crowded out by the first.
const maxImagesPerMessage = 3

// maxImageBytes caps ONE fetched file, and it is the RELAY's copy of a number the server also enforces.
//
// Both copies earn their place. This one is what keeps a 100 MB "screenshot" from being pulled into this
// process's memory and pushed across the network before anything looks at it; POST /v1/artifacts refuses
// the same size at the door because a server may not trust a client to have checked. Equal on purpose: a
// relay ceiling ABOVE the server's would fetch bytes that can only be rejected, and one BELOW it would
// refuse images the platform would have accepted.
const maxImageBytes = 5 << 20 // 5 MiB

// ArtifactCreator is the one SDK call the image leg makes: bytes in, artifact id out. It is a narrow seam
// for the same reason ThreadStore and EventStream are — a test substitutes it with no HTTP round trip, and
// this file names exactly what it needs from a client that can do far more.
type ArtifactCreator interface {
	CreateArtifact(ctx context.Context, content []byte) (string, error)
}

// NewArtifactCreator adapts a real SDK client to the one call the image leg makes. It mirrors
// NewPalaiClient's shape: production wraps the client, a test substitutes its own fake.
func NewArtifactCreator(c *palai.Client) ArtifactCreator { return artifactClient{c} }

type artifactClient struct{ c *palai.Client }

func (a artifactClient) CreateArtifact(ctx context.Context, content []byte) (string, error) {
	art, err := a.c.Artifacts.Create(ctx, content)
	if err != nil {
		return "", err
	}
	return art.ID, nil
}

// ImageLeg is the optional half of InboundDeps that turns shared files into artifact ids: a Slack HTTP
// client, the bot token those fetches present, and the Palai surface the bytes land in.
//
// IT IS OPTIONAL AND NIL-SAFE, and that posture is inherited on purpose from the path this replaces: with
// no leg mounted, a message carrying a screenshot is relayed EXACTLY as it was before this file existed —
// its text alone, no fetch, no note — so a deployment that has not configured one is unchanged rather than
// broken.
//
// THE ABSENCE IS NOT SILENT, though, and that too is inherited from a real defect: the old leg was built,
// tested, then mounted behind a nil check in a deployment that never configured its object store, and the
// only evidence anywhere that the capability was dead sat inside the run's own prompt. An operator had to
// read a model's input to discover a feature did not exist. Ready() is what a composition root logs at
// boot so that cannot happen twice.
type ImageLeg struct {
	Doer      slack.Doer
	Token     []byte
	Artifacts ArtifactCreator
}

// Ready reports whether a shared screenshot can become something the model sees at all. A leg missing any
// of its three halves cannot fetch, and says so at boot rather than per message.
func (l *ImageLeg) Ready() bool {
	return l != nil && l.Doer != nil && len(l.Token) > 0 && l.Artifacts != nil
}

// attach fetches the images a message shared, uploads each as an artifact, and returns the ids a run's
// input will name plus how many files were NOT attached.
//
// A FAILED FETCH IS NOT A FAILED TURN. Every error path here increments `skipped` and continues, and the
// caller turns that count into a sentence in the prompt. The alternative — refusing the message — means a
// Slack user can wedge their own conversation by attaching a file the bot happens not to be able to read,
// and the human's actual question goes unanswered because of a picture.
//
// The error is logged with the file id and the reason; the token is in neither, by construction — nothing
// in slack.FetchImage puts a credential in an error.
func (l *ImageLeg) attach(ctx context.Context, files []slack.SharedFile, logf func(string, ...any)) (ids []string, skipped int) {
	candidates, skipped := slack.ImageCandidates(files, maxImagesPerMessage, maxImageBytes)
	if len(candidates) == 0 {
		return nil, skipped
	}
	if !l.Ready() {
		// No leg wired: every candidate is unattached and the prompt says so. NOT logged per message — a
		// permanent configuration fact belongs in one boot line, not buried under traffic.
		return nil, skipped + len(candidates)
	}
	for _, file := range candidates {
		image, err := slack.FetchImage(ctx, l.Doer, l.Token, file, maxImageBytes)
		if err != nil {
			logf("slack-bot: not attaching file %s: %v", file.ID, err)
			skipped++
			continue
		}
		id, err := l.Artifacts.CreateArtifact(ctx, image.Content)
		if err != nil {
			logf("slack-bot: not attaching file %s: the artifact upload failed: %v", file.ID, err)
			skipped++
			continue
		}
		ids = append(ids, id)
	}
	return ids, skipped
}

// runInput builds the turn's `input` — the WHOLE reason this file exists, since an artifact nothing names
// is an artifact no model sees.
//
// WITH NO IMAGES IT RETURNS THE BARE STRING, byte-identical to every input this relay produced before the
// image leg existed. That is not an optimisation, it is the guarantee that a message carrying no picture
// takes exactly the path it always took.
//
// WITH IMAGES it returns the §25.10 content array, and the ORDER IS THE HUMAN'S WORDS FIRST: it matches
// every vision example a provider publishes, and it keeps what the person actually asked ahead of anything
// else in the turn.
func runInput(text string, artifactIDs []string, skipped int) any {
	text += imageNote(len(artifactIDs), skipped)
	if len(artifactIDs) == 0 {
		return text
	}
	items := make([]any, 0, len(artifactIDs)+1)
	items = append(items, map[string]any{"type": "input_text", "text": text})
	for _, id := range artifactIDs {
		items = append(items, map[string]any{"type": "image_ref", "artifact_id": id})
	}
	return items
}

// imageNote is the VISIBLE half of every refusal this leg makes: a file that was too big, was not an image,
// could not be fetched, or fell past the per-message cap is COUNTED into the prompt, so the model can say
// something true about it instead of answering as though nothing was attached.
//
// IT NAMES NO FILE AND NO REASON. A filename is uploader-controlled text and has no business being quoted
// into a conversation; a reason is operator detail that belongs in the log line above. What is left is a
// fixed sentence and an integer — this note is entirely ours, with no field of the payload in it, so there
// is nothing here for a user to write.
func imageNote(attached, skipped int) string {
	switch {
	case skipped == 0:
		return ""
	case attached == 0 && skipped == 1:
		return "\n\n(a file shared with this message could not be attached, so it is not visible in this conversation)"
	case attached == 0:
		return fmt.Sprintf("\n\n(%d files shared with this message could not be attached, so they are not visible in this conversation)", skipped)
	case skipped == 1:
		return "\n\n(one further file shared with this message could not be attached and is not visible in this conversation)"
	default:
		return fmt.Sprintf("\n\n(%d further files shared with this message could not be attached and are not visible in this conversation)", skipped)
	}
}

// steerImageNote is what a picture attached to a message that STEERS a still-running turn gets instead of
// an image_ref, and it is an honest report of a real ceiling rather than a design.
//
// THE COMMAND CONTRACT CARRIES A STRING. A steer is POST /v1/sessions/{id}/commands with
// {kind:send_message, delivery:steer, message:"…"} — `message` is a string on the wire and in the SDK
// (palai.SteerParams), so there is nowhere in that request for a content array to go and no artifact id it
// could carry. Widening it means changing the command contract, which is the durable command spine's
// surface and not this relay's to change.
//
// So the image is NOT SILENTLY DROPPED: the steered text says a picture arrived that the model cannot see,
// which lets it ask rather than answer confidently about something it was never shown. The bytes are not
// fetched at all in this case — paying for a download nothing can reference would be a cost with no
// reader.
const steerImageNote = "\n\n(a file was shared with this message; it cannot be attached to a turn that is already running and is not visible in this conversation)"
