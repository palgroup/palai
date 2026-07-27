package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
)

// THE SLACK IMAGE LEG (E20, spec §36) — what SLK-005 classified and deferred: the bot fetches a shared image
// and the run's input carries it, so the model can see what somebody put in the channel.
//
// It is NOT a Slack feature. The seeing is a capability of a RUN — an `image_ref` content item in the run's
// input, resolved to bytes control-plane-side (execution.decodeContentParts). All Slack does is FEED it, and
// everything in this file is either the fetch's scope or the input's shape.
//
// THE ORDERING IS THE DIFFICULT PART, and it is worth reading before changing anything here:
//
//	fetch the bytes → write the artifact (id derived from the event, run_id still NULL)
//	  → build the input naming that artifact → ADMIT → attach the artifact to the born run
//
// Every arrow is forced:
//
//   - THE ID IS DERIVED FROM THE EVENT, not minted. The input names the artifact and slackRequestHash hashes
//     the input, so a random id would make Slack's redelivery of one event hash differently from the original
//     — and the idempotency reservation would answer CONFLICT where SLK-002 requires a REPLAY. A derived id
//     makes the whole input a pure function of the event again.
//   - THE ARTIFACT LANDS BEFORE THE ADMISSION. AdmitResponse commits the run's dispatch outbox inside its own
//     transaction ("Nothing is dispatched here: the outbox row is the post-commit handoff"), so a row written
//     after it races the model step that reads it — the image would intermittently be "no longer available".
//   - SO run_id IS NULL AT FIRST. artifacts.run_id references runs(id), and the run does not exist yet.
//   - AND IT IS ATTACHED AFTERWARDS, because retention reaches an artifact through its run (§22.2's purge
//     joins artifacts to runs). An unattached row is a row data retention cannot see.
//
// CEILING, named rather than discovered later: if the process dies between the write and the attach, the row
// stays unattached and its bytes outlive the response's purge. A redelivery re-derives the same id and lands
// on the same row, so the Events-API path self-heals; Socket Mode has no redelivery and does not. Closing it
// properly means writing the artifact INSIDE the admission transaction, which is a change to api.AdmitRequest.

// slackMaxImagesPerMessage caps how many images ONE Slack message contributes. Slack itself allows ten files
// per message, so this is a REFUSAL of some of them and the prompt says so (see slackImageNote) — a silent
// drop would have the model answer about three screenshots while the user asked about five.
//
// It is deliberately below execution.maxRunImages (8): that cap is over the whole conversation, so leaving
// headroom means a second message in a thread can still attach something.
const slackMaxImagesPerMessage = 3

// slackMaxImageBytes caps ONE fetched file. A Slack screenshot is a few hundred KB; the ceiling is generous
// for a real screenshot and firm against a 100 MB "image", which is both a memory cost here and a token bill
// at the provider. Kept at or below execution.maxImageBytes so the fetch never stores something the dispatch
// would then refuse.
const slackMaxImageBytes = 5 << 20 // 5 MiB

// InboundArtifactStore is the object-store write path this bridge persists a fetched image through — the E09
// artifact seam, reused rather than a second download-and-store path. Two methods, in the order they are
// called; see the ordering argument above for why they are separate.
type InboundArtifactStore interface {
	WriteInboundArtifact(ctx context.Context, org, project, artifactID string, content []byte, mediaType string, provenance map[string]any) error
	AttachArtifactRun(ctx context.Context, org, project, artifactID, runID string) error
}

// WithFileFetch mounts the image leg: an HTTP client for Slack's file hosts and the artifact store the bytes
// land in. doer is separate from WithDecisions' client on purpose — this one talks to files.slack.com, not to
// the Web API — though the same client serves both.
//
// Nil-safe both ways, and this is the posture every optional half of this bridge takes: without it a shared
// file is admitted exactly as it was before (the message's text alone, no note, no fetch), so a deployment
// with no object store is unchanged rather than broken.
func (a *SlackAdmitter) WithFileFetch(doer slack.Doer, store InboundArtifactStore) *SlackAdmitter {
	a.fileDoer = doer
	a.inboundArtifacts = store
	return a
}

// slackImageAttachment is one image this event contributed to the run's input.
type slackImageAttachment struct {
	artifactID string
}

// admitImages fetches the images a verified, admitted message shared and persists them, returning the
// artifact ids the run input will name and how many files were NOT attached.
//
// WHY IT IS SAFE TO FETCH AT ALL, restated at the point of use because this is where the token is spent: the
// caller has already verified the signature over this body, resolved the connection from the signing secret,
// passed the channel against allowed_channels and decided the event births a run. A file id that merely
// appears in some payload therefore never reaches here. That matters more than it looks: the bot holds
// `files:read`, so a fetch driven by an id somebody else chose is a read primitive over the workspace — the
// confused-deputy shape E20 T3 refused for context entities, and the same refusal applies to file ids.
//
// A FAILED FETCH IS NOT A FAILED ADMISSION. The run is born either way and the input SAYS an image could not
// be read, because the alternative — refusing the message — means a Slack user can wedge their own
// conversation by attaching a file the bot cannot get. The failure is logged with the file id and the reason;
// the token is never in either.
func (a *SlackAdmitter) admitImages(ctx context.Context, conn api.SlackConnectionRef, ev slack.Event) ([]slackImageAttachment, int) {
	candidates, skipped := slack.ImageCandidates(ev.Files, slackMaxImagesPerMessage, slackMaxImageBytes)
	if len(candidates) == 0 {
		return nil, skipped
	}
	if a.fileDoer == nil || a.inboundArtifacts == nil || a.secrets == nil || conn.BotTokenRef == "" {
		// No fetch path wired (or no bot token on the connection): every file is unattached, and the prompt
		// will say so rather than leaving the model to guess why it cannot see anything.
		return nil, skipped + len(candidates)
	}
	token, err := a.secrets(conn.Org, conn.BotTokenRef)
	if err != nil || len(token) == 0 {
		// The ref NAME is not logged and never echoed: an unresolvable handle must not become a config oracle,
		// the same rule VerifySignature follows.
		log.Printf("slack: cannot fetch %d shared file(s) for connection %s — the bot token handle did not redeem", len(candidates), conn.ID)
		return nil, skipped + len(candidates)
	}

	var attached []slackImageAttachment
	for _, file := range candidates {
		image, err := slack.FetchImage(ctx, a.fileDoer, token, file, slackMaxImageBytes)
		if err != nil {
			// The error carries the file id and the reason (over-size, not-an-image, untrusted host, HTTP
			// status) and by construction no credential — FetchImage never puts the token in one.
			log.Printf("slack: not attaching file %s from connection %s: %v", file.ID, conn.ID, err)
			skipped++
			continue
		}
		artifactID := slackImageArtifactID(conn.ID, ev.TeamID, file.ID)
		if err := a.inboundArtifacts.WriteInboundArtifact(ctx, conn.Org, conn.Project, artifactID, image.Content, image.MediaType,
			slackImageProvenance(conn, ev, file, image)); err != nil {
			log.Printf("slack: not attaching file %s from connection %s: persist failed: %v", file.ID, conn.ID, err)
			skipped++
			continue
		}
		attached = append(attached, slackImageAttachment{artifactID: artifactID})
	}
	return attached, skipped
}

// attachImagesToRun binds each written artifact to the run the admission created, which is what puts the
// bytes inside retention's reach. Failures are logged and swallowed: the run is already durable and the
// answer is already on its way, so failing the ack here would earn a redelivery that can only replay.
func (a *SlackAdmitter) attachImagesToRun(ctx context.Context, conn api.SlackConnectionRef, images []slackImageAttachment, runID string) {
	if a.inboundArtifacts == nil {
		return
	}
	for _, img := range images {
		if err := a.inboundArtifacts.AttachArtifactRun(ctx, conn.Org, conn.Project, img.artifactID, runID); err != nil {
			log.Printf("slack: artifact %s stayed unattached to run %s — data retention will not reach it: %v", img.artifactID, runID, err)
		}
	}
}

// slackImageArtifactID derives an artifact id that is a PURE FUNCTION of the source file's identity, which is
// what keeps a redelivery a replay: the id enters the run input, the input is hashed into the idempotency
// reservation, and a minted id would hash differently the second time (see the ordering argument above).
//
// The connection is in the digest as well as the workspace and the file, so the id cannot collide across
// tenants and one workspace cannot pre-compute a row id in another's namespace — a tenant's artifact rows are
// RLS-scoped regardless, this simply keeps the primary key from being guessable-and-shared.
//
// 16 bytes of SHA-256, matching newArtifactID's width and the "art_" prefix the spine uses everywhere.
func slackImageArtifactID(connectionID, teamID, fileID string) string {
	sum := sha256.Sum256([]byte("slack-image\x00" + connectionID + "\x00" + teamID + "\x00" + fileID))
	return "art_" + hex.EncodeToString(sum[:16])
}

// slackImageProvenance records where these bytes came from (spec §22.6 provenance) — the source family, the
// connection, the workspace, Slack's file id and the SNIFFED media type.
//
// WHAT IS NOT IN IT: the bot token, obviously, but also the download URL and the FILENAME. The URL is
// credential-adjacent and expires; the filename is uploader-controlled text, and this map is read back by
// operators and served over the artifact retrieval API, which is exactly the "relay renders untrusted bytes"
// finding (E17 T10) — so a name nobody needs is a name that does not go in.
func slackImageProvenance(conn api.SlackConnectionRef, ev slack.Event, file slack.SharedFile, image slack.FetchedImage) map[string]any {
	return map[string]any{
		"source":          slack.Source,
		"connection_id":   conn.ID,
		"team_id":         ev.TeamID,
		"slack_file_id":   file.ID,
		"source_event_id": ev.SourceEventID,
		"media_type":      image.MediaType,
		"size_bytes":      len(image.Content),
	}
}

// slackImageNote is the VISIBLE half of every refusal this leg makes. A file that was too big, was not an
// image, could not be fetched, or fell past the per-message cap is COUNTED into the prompt, so the model can
// say something true about it instead of answering as though the user attached nothing.
//
// It says nothing about WHICH file or WHY: a filename is untrusted text and a reason is operator detail that
// belongs in the log line, not in a conversation.
//
// IT TRAILS THE HUMAN'S WORDS, which is the opposite of what slackContextNote does, and the difference is the
// reason rather than an inconsistency. The context note LEADS because it is UNTRUSTED — a workspace member
// puts channels in it simply by looking at them — and untrusted annotation must never sit where the most
// recent instruction goes. This note is entirely ours: a fixed sentence and an integer, with no field of the
// payload in it. There is nothing here for a user to write.
func slackImageNote(attached []slackImageAttachment, skipped int) string {
	switch {
	case skipped == 0:
		return ""
	case len(attached) == 0 && skipped == 1:
		return "\n\n(a file shared with this message could not be attached, so it is not visible in this conversation)"
	case len(attached) == 0:
		return fmt.Sprintf("\n\n(%d files shared with this message could not be attached, so they are not visible in this conversation)", skipped)
	case skipped == 1:
		return "\n\n(one further file shared with this message could not be attached and is not visible in this conversation)"
	default:
		return fmt.Sprintf("\n\n(%d further files shared with this message could not be attached and are not visible in this conversation)", skipped)
	}
}
