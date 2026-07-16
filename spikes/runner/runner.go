package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const RunnerProtocolV1 = "runner.v1"

var immutableDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Runner struct {
	ID            string
	ControllerURL string
	TLSConfig     *tls.Config
}

type ImageIdentity struct {
	Repository string `json:"repository"`
	ID         string `json:"id"`
	Digest     string `json:"digest"`
	Platform   string `json:"platform"`
}

type LeaseLimits struct {
	WallTimeMS      int64 `json:"wall_time_ms"`
	MaxStdoutBytes  int64 `json:"max_stdout_bytes"`
	MaxStderrBytes  int64 `json:"max_stderr_bytes"`
	MaxFrameBytes   int64 `json:"max_frame_bytes"`
	MaxMemoryBytes  int64 `json:"max_memory_bytes"`
	MaxProcessCount int64 `json:"max_process_count"`
}

type Lease struct {
	Protocol  string        `json:"protocol"`
	Type      string        `json:"type"`
	RunnerID  string        `json:"runner_id"`
	RunID     string        `json:"run_id"`
	AttemptID string        `json:"attempt_id"`
	Fence     uint64        `json:"fence"`
	Image     ImageIdentity `json:"image"`
	Deadline  time.Time     `json:"deadline"`
	Limits    LeaseLimits   `json:"limits"`
}

func (r Runner) ReceiveLease(ctx context.Context) (Lease, error) {
	if r.ID == "" || r.ControllerURL == "" || r.TLSConfig == nil {
		return Lease{}, errors.New("runner identity, controller URL and TLS config are required")
	}
	if !strings.HasPrefix(r.ControllerURL, "wss://") {
		return Lease{}, errors.New("runner controller URL must use wss")
	}
	transport := &http.Transport{
		TLSClientConfig: r.TLSConfig.Clone(),
		Proxy:           nil,
	}
	defer transport.CloseIdleConnections()
	connection, _, err := websocket.Dial(ctx, r.ControllerURL, &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: transport},
		Subprotocols: []string{RunnerProtocolV1},
	})
	if err != nil {
		return Lease{}, fmt.Errorf("connect outbound to controller: %w", err)
	}
	defer connection.CloseNow()
	connection.SetReadLimit(64 * 1024)
	hello, err := json.Marshal(runnerHello{Protocol: RunnerProtocolV1, Type: "runner.hello", RunnerID: r.ID})
	if err != nil {
		return Lease{}, fmt.Errorf("encode runner hello: %w", err)
	}
	if err := connection.Write(ctx, websocket.MessageText, hello); err != nil {
		return Lease{}, fmt.Errorf("write runner hello: %w", err)
	}
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		return Lease{}, fmt.Errorf("read lease offer: %w", err)
	}
	if messageType != websocket.MessageText {
		return Lease{}, errors.New("lease offer must be a text message")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var lease Lease
	if err := decoder.Decode(&lease); err != nil {
		return Lease{}, fmt.Errorf("decode lease offer: %w", err)
	}
	if !decoderAtEOF(decoder) {
		return Lease{}, errors.New("decode lease offer: trailing JSON value")
	}
	now := time.Now()
	if r.TLSConfig.Time != nil {
		now = r.TLSConfig.Time()
	}
	if err := lease.Validate(now); err != nil {
		return Lease{}, fmt.Errorf("validate lease offer: %w", err)
	}
	if lease.RunnerID != r.ID {
		return Lease{}, errors.New("lease runner identity mismatch")
	}
	_ = connection.Close(websocket.StatusNormalClosure, "lease received")
	return lease, nil
}

func (lease Lease) Validate(now time.Time) error {
	if lease.Protocol != RunnerProtocolV1 || lease.Type != "lease.offer" {
		return errors.New("unsupported lease protocol or type")
	}
	if lease.RunnerID == "" || lease.RunID == "" || lease.AttemptID == "" || lease.Fence == 0 {
		return errors.New("lease identities and fence are required")
	}
	if lease.Image.Repository == "" || lease.Image.Platform == "" ||
		!immutableDigestPattern.MatchString(lease.Image.ID) ||
		!immutableDigestPattern.MatchString(lease.Image.Digest) {
		return errors.New("lease image must include immutable ID and digest")
	}
	if now.IsZero() || !lease.Deadline.After(now) {
		return errors.New("lease deadline must be in the future")
	}
	if err := lease.Limits.Validate(); err != nil {
		return err
	}
	return nil
}

func (limits LeaseLimits) Validate() error {
	if limits.WallTimeMS <= 0 || limits.MaxStdoutBytes <= 0 || limits.MaxStderrBytes <= 0 ||
		limits.MaxFrameBytes <= 0 || limits.MaxMemoryBytes <= 0 || limits.MaxProcessCount <= 0 {
		return errors.New("all lease resource and output bounds must be positive")
	}
	if limits.MaxFrameBytes > limits.MaxStdoutBytes {
		return errors.New("frame bound cannot exceed stdout bound")
	}
	return nil
}

func decoderAtEOF(decoder *json.Decoder) bool {
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}
