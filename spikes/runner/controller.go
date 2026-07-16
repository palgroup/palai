package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

const runnerWebSocketPath = "/v1/runner/connect"

type ControllerConfig struct {
	TLSConfig        *tls.Config
	ExpectedRunner   string
	ExpectedProtocol string
	Lease            Lease
}

type Controller struct {
	server   *http.Server
	listener net.Listener
	done     chan struct{}
	serveErr error
	mu       sync.Mutex
}

func StartController(configuration ControllerConfig) (*Controller, error) {
	if configuration.TLSConfig == nil || configuration.ExpectedRunner == "" || configuration.ExpectedProtocol == "" {
		return nil, errors.New("controller TLS config, runner identity and protocol are required")
	}
	if err := configuration.Lease.Validate(configuration.TLSConfig.Time()); err != nil {
		return nil, fmt.Errorf("validate controller lease: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for runner connection: %w", err)
	}
	controller := &Controller{listener: listener, done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc(runnerWebSocketPath, controller.runnerHandler(configuration))
	controller.server = &http.Server{
		Handler:   mux,
		TLSConfig: configuration.TLSConfig.Clone(),
		ErrorLog:  log.New(io.Discard, "", 0),
	}
	go func() {
		err := controller.server.ServeTLS(listener, "", "")
		controller.mu.Lock()
		if !errors.Is(err, http.ErrServerClosed) {
			controller.serveErr = err
		}
		controller.mu.Unlock()
		close(controller.done)
	}()
	return controller, nil
}

func (c *Controller) URL() string {
	return "wss://" + c.listener.Addr().String() + runnerWebSocketPath
}

func (c *Controller) Close(ctx context.Context) error {
	shutdownErr := c.server.Shutdown(ctx)
	select {
	case <-c.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	c.mu.Lock()
	serveErr := c.serveErr
	c.mu.Unlock()
	return errors.Join(shutdownErr, serveErr)
}

func (c *Controller) runnerHandler(configuration ControllerConfig) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
			Subprotocols: []string{configuration.ExpectedProtocol},
		})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		connection.SetReadLimit(8 * 1024)
		if connection.Subprotocol() != configuration.ExpectedProtocol {
			_ = connection.Close(websocket.StatusPolicyViolation, "runner protocol required")
			return
		}
		messageType, payload, err := connection.Read(request.Context())
		if err != nil || messageType != websocket.MessageText {
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		var hello runnerHello
		if err := decoder.Decode(&hello); err != nil || !decoderAtEOF(decoder) {
			_ = connection.Close(websocket.StatusInvalidFramePayloadData, "invalid runner hello")
			return
		}
		if hello.Protocol != configuration.ExpectedProtocol || hello.Type != "runner.hello" || hello.RunnerID != configuration.ExpectedRunner {
			_ = connection.Close(websocket.StatusPolicyViolation, "runner identity mismatch")
			return
		}
		payload, err = json.Marshal(configuration.Lease)
		if err != nil {
			return
		}
		_ = connection.Write(request.Context(), websocket.MessageText, payload)
	}
}

type runnerHello struct {
	Protocol string `json:"protocol"`
	Type     string `json:"type"`
	RunnerID string `json:"runner_id"`
}
