//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// THE ARTIFACT UPLOAD LEG over the SHIPPED route and real PostgreSQL (E22 T5, SLK-012). A signed mention
// births a real run, the run reaches terminal through the SAME ApplyRunTransition production uses, and its
// answer — which names an artifact through the closed union's file_ref variant — reaches the thread as words
// AND as a real file.
//
// The only fakes are Slack itself and the object store. Everything between the HTTP request and
// files.completeUploadExternal is the production router, the production admission, the production terminal
// transaction, a real slack_reply_deliveries row and the production pump.

// uploadArtifactID is the id the model's answer names. It is written out rather than generated because the
// answer below is a literal, and the shape ("art_" + 32 hex) is the thing artifactIDFromRef recognises.
const uploadArtifactID = "art_c0ffee0123456789c0ffee0123456789"

// componentQuickTime is a QuickTime container header — an `ftyp` box whose MAJOR BRAND is `qt  `. It has to
// be a real one: the extension is SNIFFED, and the whole point of X2 is that a recording is QuickTime even
// when a model calls it .mp4.
var componentQuickTime = append([]byte{
	0x00, 0x00, 0x00, 0x14, 'f', 't', 'y', 'p', 'q', 't', ' ', ' ',
	0x00, 0x00, 0x02, 0x00, 'q', 't', ' ', ' ',
}, make([]byte, 128)...)

// fileRefAnswer is a model answer in the closed union: prose plus a file_ref naming an artifact by its
// RETRIEVAL URL. The label is deliberately hostile — it carries a broadcast token and a name that lies about
// the container — because both are things exactly one field is allowed to carry, defused.
func fileRefAnswer(id string) map[string]any {
	return map[string]any{"output": []any{map[string]any{"type": "message", "content": `[
	  {"type":"text","text":"The recording is attached."},
	  {"type":"file_ref","url":"https://palai.test/v1/artifacts/` + id + `/content","label":"demo.mp4 <!channel>"}
	]`}}}
}

// TestSlackRunArtifactReachesTheThreadAsAFile is the leg end to end.
func TestSlackRunArtifactReachesTheThreadAsAFile(t *testing.T) {
	f := newSlackFixture(t)
	ctx := context.Background()

	f.deliver(t, f.eventText(t, "EvU1", "app_mention", "Umapped", "C95", "1700000095.000100", "", "<@"+f.botUser+"> record the app"),
		time.Now(), "", "").Body.Close()
	runID, responseID, _ := f.runAndResponse(t)

	// The run produced a screen recording. In production the bytes are in the object store under the run that
	// made them; here the fake holds them under the same two keys.
	f.artifacts.putRunArtifact(f.org, f.project, runID, uploadArtifactID, componentQuickTime, 0)

	f.finalizeWith(t, responseID, "completed", fileRefAnswer(uploadArtifactID))
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart, statemachines.RunCmdComplete)

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(ctx); err != nil || posted != 1 {
		t.Fatalf("the pump posted %d replies (err %v), want 1", posted, err)
	}

	// THE ANSWER LANDED FIRST. The upload is not allowed to be a precondition of the run's own answer.
	posts := f.postCalls()
	if len(posts) != 1 {
		t.Fatalf("fake Slack saw %d chat.postMessage call(s), want 1", len(posts))
	}

	// THE THREE STEPS RAN, IN ORDER, AFTER THE POST.
	var order []string
	for _, c := range f.slackCalls() {
		switch {
		case c.path == "/chat.postMessage", c.path == "/files.getUploadURLExternal", c.path == "/files.completeUploadExternal":
			order = append(order, c.path)
		case strings.HasPrefix(c.path, "/upload/v1/"):
			order = append(order, "/upload")
		}
	}
	want := []string{"/chat.postMessage", "/files.getUploadURLExternal", "/upload", "/files.completeUploadExternal"}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Fatalf("the call order was %v, want %v — the answer posts first (SLK-006) and the three upload steps follow in the documented order", order, want)
	}

	// THE RESERVATION NAMES THE BYTES, NOT THE MODEL. `.mov`, because the container is QuickTime — the model
	// called it demo.mp4 and that name reaches nothing.
	reserve := f.callsTo("/files.getUploadURLExternal")[0]
	var step1 map[string]any
	if err := json.Unmarshal([]byte(reserve.body), &step1); err != nil {
		t.Fatalf("decode the upload reservation: %v (%s)", err, reserve.body)
	}
	if step1["filename"] != uploadArtifactID+".mov" {
		t.Fatalf("the reservation named %v, want %s.mov derived from the bytes", step1["filename"], uploadArtifactID)
	}
	if length, _ := step1["length"].(float64); int(length) != len(componentQuickTime) {
		t.Fatalf("the reservation declared length %v, want %d", step1["length"], len(componentQuickTime))
	}
	if reserve.auth != "Bearer "+string(f.botToken) {
		t.Fatalf("the reservation went out with auth %q, want the redeemed bot token", reserve.auth)
	}

	// THE BYTES WENT UP VERBATIM, TO THE URL SLACK RETURNED, WITH NO CREDENTIAL.
	var uploaded *slackCall
	for i, c := range f.slackCalls() {
		if strings.HasPrefix(c.path, "/upload/v1/") {
			uploaded = &f.slackCalls()[i]
		}
	}
	if uploaded == nil {
		t.Fatal("the bytes were never posted to the upload url")
	}
	if uploaded.body != string(componentQuickTime) {
		t.Fatalf("the upload carried %d byte(s), want the artifact's %d verbatim", len(uploaded.body), len(componentQuickTime))
	}
	if uploaded.auth != "" {
		t.Fatalf("the byte upload carried an Authorization header (%q); the upload url is already the authorization", uploaded.auth)
	}

	// THE FILE WAS SHARED INTO THE THREAD THE QUESTION WAS ASKED IN, at its PARENT ts — the value
	// slack_reply_deliveries froze at enqueue, with no new column.
	complete := f.callsTo("/files.completeUploadExternal")[0]
	var step3 struct {
		Files          []map[string]any `json:"files"`
		ChannelID      string           `json:"channel_id"`
		Channels       string           `json:"channels"`
		ThreadTS       string           `json:"thread_ts"`
		InitialComment string           `json:"initial_comment"`
		Blocks         any              `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(complete.body), &step3); err != nil {
		t.Fatalf("decode the upload completion: %v (%s)", err, complete.body)
	}
	if step3.ChannelID != "C95" || step3.ThreadTS != "1700000095.000100" || step3.Channels != "" {
		t.Fatalf("the file was shared to channel_id=%q channels=%q thread_ts=%q, want C95 / '' / the parent ts",
			step3.ChannelID, step3.Channels, step3.ThreadTS)
	}
	if step3.Blocks != nil {
		t.Fatalf("the completion sent `blocks` (%v); it is not documented to accept a markdown block", step3.Blocks)
	}
	if len(step3.Files) != 1 {
		t.Fatalf("the completion named %d file(s), want 1", len(step3.Files))
	}

	// EXACTLY ONE FIELD CARRIES THE MODEL'S WORDS — across EVERY body the fake saw, not just the upload's —
	// and the sweep JSON-DECODES first, because encoding/json escapes `<` and a raw-substring assertion over
	// marshalled bytes could never fail (E20 T4).
	carriers := map[string]int{}
	for _, c := range f.slackCalls() {
		var decoded any
		if err := json.Unmarshal([]byte(c.body), &decoded); err != nil {
			continue
		}
		walkComponentStrings(decoded, "", func(field, value string) {
			if strings.Contains(value, "demo.mp4") {
				carriers[c.path+"."+field]++
			}
		})
	}
	if carriers["/files.completeUploadExternal.initial_comment"] != 1 {
		t.Fatalf("the model's label reached %v; it must reach initial_comment exactly once", carriers)
	}
	for field := range carriers {
		if !strings.HasSuffix(field, ".initial_comment") && !strings.HasPrefix(field, "/chat.postMessage") {
			t.Fatalf("the model's label reached %q; only initial_comment (and the answer itself) may carry it", field)
		}
	}
	// And it was DEFUSED on the way: the broadcast token in the model's label is characters, not a ping.
	if strings.Contains(step3.InitialComment, "<!channel>") {
		t.Fatalf("a live broadcast token survived into initial_comment: %q", step3.InitialComment)
	}
	if !strings.Contains(step3.InitialComment, "channel") {
		t.Fatalf("the label vanished instead of being defused: %q — the assertion above would then prove nothing", step3.InitialComment)
	}

	// The run is untouched and the delivery is settled exactly once.
	if state, _ := f.replyState(t, runID); state != "delivered" {
		t.Fatalf("delivery state = %q, want delivered", state)
	}
	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(ctx); err != nil || posted != 0 {
		t.Fatalf("a second tick posted %d (err %v), want 0 — files.completeUploadExternal may only be called once", posted, err)
	}
	if n := len(f.callsTo("/files.completeUploadExternal")); n != 1 {
		t.Fatalf("the file was completed %d time(s), want 1", n)
	}
}

// ANOTHER RUN'S ARTIFACT IS NOT THIS THREAD'S TO PUBLISH. The id in the answer is a string a MODEL wrote, and
// the tenant alone is not a boundary here: same org, same project, different conversation. Without the run in
// the key, a model could name a screenshot taken for somebody else's thread and have it posted into this one.
func TestSlackUploadRefusesAnArtifactFromAnotherRun(t *testing.T) {
	f := newSlackFixture(t)

	f.deliver(t, f.eventText(t, "EvU2", "app_mention", "Umapped", "C96", "1700000096.000100", "", "<@"+f.botUser+"> show me"),
		time.Now(), "", "").Body.Close()
	runID, responseID, _ := f.runAndResponse(t)

	// The artifact exists, in this very tenant — it just belongs to a DIFFERENT run.
	f.artifacts.putRunArtifact(f.org, f.project, "run_somebody_elses", uploadArtifactID, componentQuickTime, 0)

	f.finalizeWith(t, responseID, "completed", fileRefAnswer(uploadArtifactID))
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart, statemachines.RunCmdComplete)

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 1 {
		t.Fatalf("the pump posted %d replies (err %v), want 1 — the answer still lands", posted, err)
	}
	if n := len(f.callsTo("/files.getUploadURLExternal")); n != 0 {
		t.Fatalf("a foreign run's artifact produced %d upload attempt(s), want 0", n)
	}
	// The answer says so rather than being silent about it, and it does not say WHICH id — that would be an
	// existence oracle over another run's artifacts.
	body := f.postCalls()[0].body
	if !strings.Contains(body, "could not be attached") {
		t.Fatalf("the answer said nothing about the file it could not attach: %s", body)
	}
	if strings.Contains(body, "another run") || strings.Contains(body, "belongs to") {
		t.Fatalf("the answer explained WHY a foreign artifact was refused, which tells the asker it exists: %s", body)
	}
	_ = runID
}

// AN ARTIFACT OVER OUR CEILING IS NOT UPLOADED AND NOT HIDDEN. The answer carries an honest sentence and the
// link the model wrote is still in it, so a human who wants the 40 MB recording knows both that it exists and
// why it is not sitting in the channel.
func TestSlackUploadRefusesAnOversizeArtifactAndSaysSo(t *testing.T) {
	f := newSlackFixture(t)

	f.deliver(t, f.eventText(t, "EvU3", "app_mention", "Umapped", "C97", "1700000097.000100", "", "<@"+f.botUser+"> record it all"),
		time.Now(), "", "").Body.Close()
	runID, responseID, _ := f.runAndResponse(t)

	// The ROW says 40 MiB. The bytes are never read — which is the point of checking the size on the row.
	f.artifacts.putRunArtifact(f.org, f.project, runID, uploadArtifactID, componentQuickTime, 40<<20)

	f.finalizeWith(t, responseID, "completed", fileRefAnswer(uploadArtifactID))
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart, statemachines.RunCmdComplete)

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 1 {
		t.Fatalf("the pump posted %d replies (err %v), want 1", posted, err)
	}
	if n := len(f.callsTo("/files.getUploadURLExternal")); n != 0 {
		t.Fatalf("an over-ceiling artifact produced %d upload attempt(s), want 0 — the refusal must cost nothing", n)
	}
	body := f.postCalls()[0].body
	if !strings.Contains(body, "8 MiB") || !strings.Contains(body, "over the") {
		t.Fatalf("the answer did not say the file was over the limit: %s", body)
	}
	// The link is still there: the artifact is refused as an ATTACHMENT, not erased from the answer.
	if !strings.Contains(body, uploadArtifactID) {
		t.Fatalf("the file_ref link was dropped along with the upload: %s", body)
	}
}

// SLK-006 THROUGH THE UPLOAD: a Slack that refuses the FILE cannot touch the run, cannot un-post the answer,
// and cannot make the delivery pending again — otherwise a workspace without files:write would earn a second
// copy of every answer.
func TestSlackUploadFailureLeavesTheAnswerAndTheRunAlone(t *testing.T) {
	f := newSlackFixture(t)
	ctx := context.Background()

	f.deliver(t, f.eventText(t, "EvU4", "app_mention", "Umapped", "C98", "1700000098.000100", "", "<@"+f.botUser+"> ship it"),
		time.Now(), "", "").Body.Close()
	runID, responseID, _ := f.runAndResponse(t)
	f.artifacts.putRunArtifact(f.org, f.project, runID, uploadArtifactID, componentQuickTime, 0)

	f.finalizeWith(t, responseID, "completed", fileRefAnswer(uploadArtifactID))
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart, statemachines.RunCmdComplete)

	// The workspace never granted files:write. This is the documented refusal, not a transport failure.
	f.slackRefuse("/files.getUploadURLExternal", "missing_scope")

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(ctx); err != nil || posted != 1 {
		t.Fatalf("a refused upload reported %d posted (err %v), want 1 — the ANSWER is the delivery", posted, err)
	}
	if len(f.postCalls()) != 1 {
		t.Fatalf("fake Slack saw %d posts, want the one answer", len(f.postCalls()))
	}
	if state, _ := f.replyState(t, runID); state != "delivered" {
		t.Fatalf("delivery state = %q after a refused upload, want delivered — a file is not the answer", state)
	}
	var state, projection string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT r.state, resp.output::text FROM runs r JOIN responses resp ON resp.id = r.response_id WHERE r.id=$1`,
		runID).Scan(&state, &projection); err != nil {
		t.Fatalf("read the run after a refused upload: %v", err)
	}
	if state != "completed" || !strings.Contains(projection, "file_ref") {
		t.Fatalf("run = %q with projection %q; an upload failure must never erase the canonical result", state, projection)
	}
	// And nothing retries it: re-claiming a settled row to retry a file would risk a second ANSWER.
	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(ctx); err != nil || posted != 0 {
		t.Fatalf("a tick after a refused upload posted %d (err %v), want 0", posted, err)
	}
}

// AN ANSWER WITH NO file_ref UPLOADS NOTHING. Without this the leg could be reading artifacts on every reply
// in the tree and nothing would notice.
func TestSlackOrdinaryAnswerUploadsNothing(t *testing.T) {
	f := newSlackFixture(t)

	f.deliver(t, f.eventText(t, "EvU5", "app_mention", "Umapped", "C99", "1700000099.000100", "", "<@"+f.botUser+"> merhaba"),
		time.Now(), "", "").Body.Close()
	runID, responseID, _ := f.runAndResponse(t)
	f.finalizeWith(t, responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": "Merhaba!"}},
	})
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart, statemachines.RunCmdComplete)

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 1 {
		t.Fatalf("the pump posted %d replies (err %v), want 1", posted, err)
	}
	if n := len(f.callsTo("/files.getUploadURLExternal")); n != 0 {
		t.Fatalf("an ordinary answer made %d upload call(s), want 0", n)
	}
	if reads := f.artifacts.readIDs(); len(reads) != 0 {
		t.Fatalf("an ordinary answer read %v from the object store, want nothing", reads)
	}
	if body := f.postCalls()[0].body; strings.Contains(body, "could not be attached") {
		t.Fatalf("an ordinary answer grew an upload note: %s", body)
	}
}

// walkComponentStrings visits every string in a decoded JSON body, naming it by the key it hangs off.
func walkComponentStrings(node any, key string, visit func(field, value string)) {
	switch v := node.(type) {
	case string:
		visit(key, v)
	case map[string]any:
		for k, child := range v {
			walkComponentStrings(child, k, visit)
		}
	case []any:
		for _, child := range v {
			walkComponentStrings(child, key, visit)
		}
	}
}
