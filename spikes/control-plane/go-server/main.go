package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

type counters struct {
	activeConnections atomic.Int64
	disconnects       atomic.Int64
	cancelRequests    atomic.Int64
}

func main() {
	port := flag.Int("port", 0, "loopback port; zero asks the OS to assign one")
	heartbeat := flag.Duration("heartbeat", 15*time.Second, "SSE heartbeat interval")
	flag.Parse()
	if *port < 0 || *port > 65535 || *heartbeat <= 0 {
		log.Fatal("invalid port or heartbeat")
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	rootContext, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	state := &counters{}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/stats", state.statsHandler)
	mux.HandleFunc("/events", state.eventsHandler(rootContext, *heartbeat))
	mux.HandleFunc("/jobs/fixture/cancel", state.cancelHandler)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return rootContext
		},
	}

	ready := map[string]string{
		"event":   "ready",
		"address": listener.Addr().String(),
		"runtime": "go",
	}
	if err := json.NewEncoder(os.Stdout).Encode(ready); err != nil {
		log.Fatalf("write readiness: %v", err)
	}
	log.Printf("control-plane candidate listening runtime=go address=%s", listener.Addr())

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	case received := <-signals:
		log.Printf("control-plane candidate stopping runtime=go signal=%s", received)
		cancelRoot()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := server.Shutdown(shutdownContext)
		cancel()
		if err != nil {
			log.Fatalf("shutdown: %v", err)
		}
		if err := <-serveResult; !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve after shutdown: %v", err)
		}
	}
}

func healthHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("ok\n"))
}

func (state *counters) statsHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]int64{
		"active_connections": state.activeConnections.Load(),
		"disconnects":        state.disconnects.Load(),
		"cancel_requests":    state.cancelRequests.Load(),
	})
}

func (state *counters) cancelHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	state.cancelRequests.Add(1)
	writer.WriteHeader(http.StatusAccepted)
}

func (state *counters) eventsHandler(rootContext context.Context, heartbeat time.Duration) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		lastID, err := parseLastEventID(request.Header.Get("Last-Event-ID"))
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		flusher, ok := writer.(http.Flusher)
		if !ok {
			http.Error(writer, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		state.activeConnections.Add(1)
		defer state.activeConnections.Add(-1)
		defer state.disconnects.Add(1)

		for sequence := lastID + 1; sequence <= 2; sequence++ {
			_, _ = fmt.Fprintf(writer, "id: %d\nevent: fixture\ndata: {\"sequence\":%d}\n\n", sequence, sequence)
		}
		flusher.Flush()

		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-request.Context().Done():
				return
			case <-rootContext.Done():
				return
			case <-ticker.C:
				_, _ = fmt.Fprint(writer, ": heartbeat\n\n")
				flusher.Flush()
			}
		}
	}
}

func parseLastEventID(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	lastID, err := strconv.Atoi(value)
	if err != nil || lastID < 0 || lastID > 2 {
		return 0, fmt.Errorf("invalid Last-Event-ID")
	}
	return lastID, nil
}
