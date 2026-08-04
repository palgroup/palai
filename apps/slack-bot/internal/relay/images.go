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
//
// A CEILING THAT LIVED ENTIRELY OUTSIDE THIS FILE, AND CLOSED ON 2026-08-04. It is recorded rather than
// deleted because the SHAPE of it is still true even though the instance is not: an image reaching the
// model is a property of the MODEL ROUTE, not of this leg, and for one week this deployment's route made
// every picture a failed run. prj_local's published `default` route is claude-sonnet-5 over a
// `provider-two` connection, and that adapter used to refuse ANY request carrying an image — deliberately,
// since dropping the picture and answering the text alone is the silent-wrong-answer shape §27.5 exists to
// refuse. So a Slack turn that attached a picture did not get a worse answer, it got NO answer.
//
//	$ psql … -tAc "select rr.config->>'model', c.provider from model_route_revisions rr
//	    join model_routes r on r.id = rr.route_id join model_connections c
//	      on c.id = rr.config->>'connection_id'
//	    where r.project_id = 'prj_local' and r.name = 'default' order by rr.revision desc limit 1"
//	→ claude-sonnet-5 | provider-two          (2026-08-04, unchanged)
//
// The route is the SAME; what changed is the adapter under it. provider-two now converts images to
// Anthropic image content blocks, proved by a real round trip against claude-sonnet-5 — the exact model
// above — in `PROVIDER=provider-two CASE=vision-reads-image scripts/test/live-provider`. Both direct
// provider families convert images now, so a picture attached in Slack reaches the model on this
// deployment's own route.
//
// WHAT IS STILL TRUE, and is why this paragraph survives: a route is an operator's to publish, and nothing
// in the public API tells a relay what the route it will land on can render. A future route pointing at a
// model or a family without vision would put this leg right back where it was, and this file would again be
// unable to detect it — the images would be uploaded, named, and never looked at.

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
//
// `max` is a PARAMETER rather than maxImagesPerMessage read straight from the package, because the two
// callers spend two different budgets: the triggering message's own attachments get that constant, and a
// thread's earlier pictures get their own (see history.go's maxThreadImages) so they can never take a slot
// from the message the human actually sent.
func (l *ImageLeg) attach(ctx context.Context, files []slack.SharedFile, max int, logf func(string, ...any)) (ids []string, skipped int) {
	candidates, skipped := slack.ImageCandidates(files, max, maxImageBytes)
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

// turnImages is everything one turn's pictures amount to, split by WHERE they came from — the message the
// human just sent, or the earlier messages of a thread this bot was invited into late (history.go).
//
// The split is carried this far rather than collapsed into one list plus one count because the prompt has to
// say something different about each: a file attached to the request is one the asker expects an answer
// about, and an image from three messages ago is context they did not hand over. A model told the difference
// can answer accordingly; one handed six pictures with no provenance cannot.
type turnImages struct {
	// own are the artifact ids for the triggering message's own attachments, and skipped is how many of its
	// files did not become one — for any reason, including not being an image at all.
	own     []string
	skipped int
	// earlier are the artifact ids for images shared EARLIER in the same thread, oldest first, and missing is
	// how many images this read found there and could not carry (past the budget, or a failed fetch).
	earlier []string
	missing int
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
//
// THE PICTURES THEMSELVES ARE CHRONOLOGICAL — the thread's earlier ones, then the message's own — which is
// both how a reader saw them and what decides which survive a long conversation: the control plane spends
// its per-request image budget on the LAST images in the input (execution.decodeMessages), so the message
// the human actually sent is the one that keeps its slot.
func runInput(text string, images turnImages) any {
	text += imageNote(len(images.own), images.skipped) + threadImageNote(len(images.earlier), images.missing)
	if len(images.own)+len(images.earlier) == 0 {
		return text
	}
	items := make([]any, 0, len(images.own)+len(images.earlier)+1)
	items = append(items, map[string]any{"type": "input_text", "text": text})
	for _, id := range append(append([]string(nil), images.earlier...), images.own...) {
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

// threadImageNote says where an image the human did NOT attach to this message came from.
//
// IT EXISTS BECAUSE SILENCE HERE WOULD BE A LIE OF PROVENANCE, not merely an omission. Handed a screenshot
// with no note, a model reads it as "the picture attached to this request" and answers as though the person
// pointed at it; it might be a diagram somebody else posted twenty minutes earlier about something else. The
// sentence costs one line and turns a confident wrong answer into an accurate one.
//
// IT NAMES NO FILE, NO UPLOADER AND NO TIME, for imageNote's reason: a filename and a display name are
// uploader-controlled text with no business being quoted into a conversation, and this note is entirely
// ours, with no field of the payload in it, so there is nothing here for a user to write.
//
// `missing` counts only IMAGES this read found and could not carry — never a document somebody shared in the
// thread hours ago, which was offered to nobody (see threadImages).
func threadImageNote(attached, missing int) string {
	var note string
	switch {
	case attached == 1:
		note = "\n\n(one image shared EARLIER in this Slack thread is attached to this turn: it was posted before this request, not with it"
	case attached > 1:
		note = fmt.Sprintf("\n\n(%d images shared EARLIER in this Slack thread are attached to this turn: they were posted before this request, not with it", attached)
	case missing > 0:
		return fmt.Sprintf("\n\n(%d image(s) shared earlier in this Slack thread could not be attached, so they are not visible in this conversation)", missing)
	default:
		return ""
	}
	if missing > 0 {
		note += fmt.Sprintf("; %d further image(s) from earlier in the thread could not be attached and are not visible", missing)
	}
	return note + ")"
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
