package runner

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LogLine is one line this machine wrote, as it crosses the wire.
type LogLine struct {
	At        time.Time `json:"at"`
	Level     string    `json:"level,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Message   string    `json:"message"`
}

// LogShipConfig is the transport for a shipment: the same controller trust and exact DNS identity every
// other runner-plane call pins.
type LogShipConfig struct {
	LogsURL       string
	ControllerCAs *x509.CertPool
	ControllerDNS string
	Now           func() time.Time
}

// MaxLogBatch is how many lines one shipment carries. A machine that was offline has a backlog, and the
// answer to a backlog is several shipments — one unbounded body would be a body large enough to exhaust
// the reader that accepts it, sent by the machine that is least able to notice.
const MaxLogBatch = 500

// ShipLogs sends one batch. It presents the SAME certificate and pins the SAME controller identity the
// settings poll and the renewal do, through renewTLS, because a second spelling of a TLS configuration
// is a second thing that can be weakened on its own.
//
// A REFUSAL IS NOT AN OUTAGE. Logs are diagnostics: a machine whose shipment is rejected must keep doing
// its work, so every failure here is returned for the caller to drop rather than acted on. What it must
// never do is block the agent, which is why the caller holds the buffer and this call holds nothing.
func ShipLogs(ctx context.Context, current Identity, lines []LogLine, config LogShipConfig) error {
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > MaxLogBatch {
		return fmt.Errorf("ship logs: %d lines exceeds the %d-line batch bound", len(lines), MaxLogBatch)
	}
	if current.Certificate.Leaf == nil || current.Certificate.PrivateKey == nil ||
		config.ControllerCAs == nil || config.ControllerDNS == "" || config.Now == nil {
		return errors.New("shipping logs requires the current identity, controller trust, DNS and clock")
	}
	if !strings.HasPrefix(config.LogsURL, "https://") {
		return errors.New("logs URL must be outbound https")
	}
	body, err := json.Marshal(struct {
		Lines []LogLine `json:"lines"`
	}{Lines: lines})
	if err != nil {
		return fmt.Errorf("encode log batch: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.LogsURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build log request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{TLSClientConfig: renewTLS(current, RenewConfig{
		ControllerCAs: config.ControllerCAs, ControllerDNS: config.ControllerDNS, Now: config.Now,
	}), Proxy: nil}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return fmt.Errorf("ship logs: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("log shipment rejected: %s", response.Status)
	}
	return nil
}

// LogBuffer holds what the agent has written since the last shipment.
//
// IT DROPS THE OLDEST RATHER THAN BLOCKING THE WRITER, and that direction is the whole point: this
// buffer sits behind the agent's own log output, so a full buffer that blocked would stop the machine
// doing its work in order to complain about it. A dropped line is a missing diagnostic; a blocked agent
// is a missing machine.
type LogBuffer struct {
	mu    sync.Mutex
	lines []LogLine
	cap   int
}

func NewLogBuffer(capacity int) *LogBuffer {
	if capacity <= 0 {
		capacity = MaxLogBatch * 4
	}
	return &LogBuffer{cap: capacity}
}

// Write makes the buffer an io.Writer, so it can sit under the standard log package with no agent code
// changing shape. One Write is one line; the trailing newline the logger adds is trimmed.
func (b *LogBuffer) Write(p []byte) (int, error) {
	message := strings.TrimRight(string(p), "\n")
	if strings.TrimSpace(message) == "" {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, LogLine{At: time.Now().UTC(), Message: message})
	if over := len(b.lines) - b.cap; over > 0 {
		b.lines = append([]LogLine(nil), b.lines[over:]...)
	}
	return len(p), nil
}

// Take removes and returns up to n lines, oldest first. The caller ships them; a failed shipment loses
// them, which is the trade this type exists to make explicit rather than to hide behind a retry queue
// that could grow without bound on a machine that cannot reach its plane.
func (b *LogBuffer) Take(n int) []LogLine {
	if n <= 0 || n > MaxLogBatch {
		n = MaxLogBatch
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) == 0 {
		return nil
	}
	if n > len(b.lines) {
		n = len(b.lines)
	}
	out := append([]LogLine(nil), b.lines[:n]...)
	b.lines = append([]LogLine(nil), b.lines[n:]...)
	return out
}
