package execution

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// RunnerLogSink takes the lines one machine shipped. It is an interface so the gateway does not depend
// on the store, and so a test can drive the route without a database.
type RunnerLogSink interface {
	Append(ctx context.Context, project, runnerID string, lines []RunnerLogLine) error
}

// RunnerLogLine is one line as it crosses the wire. It mirrors the store's shape rather than sharing it,
// because this one is a WIRE CONTRACT: a field added here is a field every deployed agent may send, and
// the two should be free to move apart.
type RunnerLogLine struct {
	At        time.Time `json:"at"`
	Level     string    `json:"level,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Message   string    `json:"message"`
}

// maxRunnerLogBody bounds ONE shipment on the wire. An agent reconnecting after an outage has a backlog,
// and the answer is several batches — a body large enough to hold an hour of a busy machine's logs is a
// body large enough to exhaust the reader that accepts it.
const maxRunnerLogBody = 1 << 20

// handleLogs takes what a machine says about itself.
//
// THE MACHINE IS READ FROM ITS CERTIFICATE AND NEVER FROM THE BODY. A runner id in the payload would let
// any enrolled machine write lines into another machine's history — the same identity rule every other
// route on this gateway follows, and the reason this handler decodes only lines.
//
// A REFUSAL IS NOT AN OUTAGE. Logs are diagnostics: an agent whose shipment is rejected must keep
// working, so the failure modes here are ordinary HTTP codes the agent drops on the floor after
// recording locally. What it must never do is retry forever into a wedged plane, which is why the batch
// is bounded on both sides.
func (g *RunnerGateway) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if g.logs == nil {
		// This deployment keeps no fleet log. Saying so beats accepting and discarding: an agent that
		// believes its lines landed is an operator who believes the machine said nothing.
		http.Error(w, "this deployment does not collect machine logs", http.StatusNotImplemented)
		return
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "runner client certificate required", http.StatusUnauthorized)
		return
	}
	dns := renewDNS(r.TLS.PeerCertificates[0])
	if dns == "" {
		http.Error(w, "runner certificate carries no identity", http.StatusUnauthorized)
		return
	}
	var body struct {
		Lines []RunnerLogLine `json:"lines"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRunnerLogBody)).Decode(&body); err != nil {
		http.Error(w, "invalid log batch", http.StatusBadRequest)
		return
	}
	runnerID := runnerIDFromDNS(dns)
	poolID, _, _ := g.recordSeen(r.Context(), dns, time.Time{})
	_ = poolID
	if err := g.logs.Append(r.Context(), "", runnerID, body.Lines); err != nil {
		http.Error(w, "could not record the batch", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
