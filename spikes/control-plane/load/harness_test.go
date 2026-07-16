package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type observedEvent struct {
	ID    string
	Event string
	Data  string
}

type observedStats struct {
	ActiveConnections int64 `json:"active_connections"`
	Disconnects       int64 `json:"disconnects"`
	CancelRequests    int64 `json:"cancel_requests"`
}

func TestCandidateProtocol(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatalf("repositoryRoot() error = %v", err)
	}
	candidates, err := prepareCandidates(t.Context(), root)
	if err != nil {
		t.Fatalf("prepareCandidates() error = %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(candidates))
	}

	for _, candidate := range candidates {
		t.Run(candidate.Name, func(t *testing.T) {
			process, err := startCandidate(t.Context(), candidate, 40*time.Millisecond)
			if err != nil {
				t.Fatalf("startCandidate() error = %v", err)
			}
			t.Cleanup(func() { process.forceStop() })

			response, err := http.Get(process.BaseURL + "/healthz")
			if err != nil {
				t.Fatalf("GET /healthz error = %v", err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("GET /healthz status = %d, want 200", response.StatusCode)
			}
			if process.ReadyDuration <= 0 {
				t.Fatalf("ready duration = %v, want positive", process.ReadyDuration)
			}
			assertHTTPStatus(t, http.MethodGet, process.BaseURL+"/events", "not-an-id", http.StatusBadRequest)
			assertHTTPStatus(t, http.MethodPost, process.BaseURL+"/events", "", http.StatusMethodNotAllowed)

			full := observeStream(t, process.BaseURL, "", 2, true)
			assertFixtureEvents(t, full, []string{"1", "2"})

			resumed := observeStream(t, process.BaseURL, "1", 1, true)
			assertFixtureEvents(t, resumed, []string{"2"})

			before := fetchStats(t, process.BaseURL)
			disconnectOneClient(t, process.BaseURL)
			eventually(t, time.Second, func() bool {
				after := fetchStats(t, process.BaseURL)
				return after.ActiveConnections == 0 &&
					after.Disconnects >= before.Disconnects+1 &&
					after.CancelRequests == 0
			})

			shutdownContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			started := time.Now()
			if err := process.stop(shutdownContext); err != nil {
				t.Fatalf("stop() error = %v", err)
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("shutdown duration = %v, want <= 5s", elapsed)
			}
			if diagnostics := process.stderr.String(); !strings.Contains(diagnostics, "runtime="+candidate.Name) {
				t.Fatalf("stderr diagnostics = %q, want runtime identity", diagnostics)
			}
		})
	}
}

func assertHTTPStatus(t *testing.T, method, url, lastEventID string, want int) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, url, err)
	}
	response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("%s %s status = %d, want %d", method, url, response.StatusCode, want)
	}
}

func TestReconnectQuickProfile(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatalf("repositoryRoot() error = %v", err)
	}
	candidates, err := prepareCandidates(t.Context(), root)
	if err != nil {
		t.Fatalf("prepareCandidates() error = %v", err)
	}
	profile := quickLoadProfile()
	if profile.Connections != 25 || profile.Reconnects != 10 || profile.RestartCycles != 1 || profile.MaximumErrors != 0 {
		t.Fatalf("quick profile = %+v, want 25 connections, 10 reconnects, 1 restart", profile)
	}

	for _, candidate := range candidates {
		t.Run(candidate.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			result, err := runCandidateLoad(ctx, candidate, profile)
			if err != nil {
				t.Fatalf("runCandidateLoad() error = %v", err)
			}
			if result.Connected != profile.Connections {
				t.Errorf("connected = %d, want %d", result.Connected, profile.Connections)
			}
			if result.Reconnects != profile.Reconnects {
				t.Errorf("reconnects = %d, want %d", result.Reconnects, profile.Reconnects)
			}
			if result.RestartCycles != profile.RestartCycles {
				t.Errorf("restart cycles = %d, want %d", result.RestartCycles, profile.RestartCycles)
			}
			if result.SequenceDuplicates != 0 || result.SequenceGaps != 0 || result.Errors != 0 {
				t.Errorf("sequence/error result = %+v, want all zero", result)
			}
			if result.CancelRequests != 0 {
				t.Errorf("cancel requests = %d, want 0", result.CancelRequests)
			}
			if result.ShutdownDuration > 5*time.Second {
				t.Errorf("shutdown duration = %v, want <= 5s", result.ShutdownDuration)
			}
		})
	}
}

func TestEvidenceProfileBounds(t *testing.T) {
	profile := evidenceLoadProfile()
	if profile.Connections != 1000 || profile.Reconnects != 100 || profile.RestartCycles != 1 {
		t.Fatalf("evidence profile scale = %+v", profile)
	}
	if profile.ConnectDeadline != 30*time.Second || profile.ShutdownDeadline != 5*time.Second {
		t.Fatalf("evidence profile deadlines = %+v", profile)
	}
	if profile.MaximumErrors != 0 || maximumGoIdleRSS != 128*1024*1024 || maximumNodeIdleRSS != 256*1024*1024 {
		t.Fatalf("evidence profile error/RSS bounds = %+v go=%d node=%d", profile, maximumGoIdleRSS, maximumNodeIdleRSS)
	}
}

func observeStream(t *testing.T, baseURL, lastEventID string, eventCount int, wantHeartbeat bool) []observedEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status = %d, want 200", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cacheControl)
	}

	reader := bufio.NewReader(response.Body)
	events := make([]observedEvent, 0, eventCount)
	current := observedEvent{}
	heartbeat := false
	for len(events) < eventCount || (wantHeartbeat && !heartbeat) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE line error = %v", err)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case strings.HasPrefix(line, ":"):
			heartbeat = true
		case line == "":
			if current.ID != "" || current.Event != "" || current.Data != "" {
				events = append(events, current)
				current = observedEvent{}
			}
		case strings.HasPrefix(line, "id:"):
			current.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			current.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			current.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		default:
			t.Fatalf("unexpected SSE line %q", line)
		}
	}
	return events
}

func assertFixtureEvents(t *testing.T, events []observedEvent, wantIDs []string) {
	t.Helper()
	if len(events) != len(wantIDs) {
		t.Fatalf("event count = %d, want %d: %+v", len(events), len(wantIDs), events)
	}
	for index, wantID := range wantIDs {
		wantData := fmt.Sprintf(`{"sequence":%s}`, wantID)
		if events[index].ID != wantID || events[index].Event != "fixture" || events[index].Data != wantData {
			t.Errorf("event[%d] = %+v, want id=%s event=fixture data=%s", index, events[index], wantID, wantData)
		}
	}
}

func fetchStats(t *testing.T, baseURL string) observedStats {
	t.Helper()
	response, err := http.Get(baseURL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /stats status = %d, want 200", response.StatusCode)
	}
	var stats observedStats
	if err := json.NewDecoder(response.Body).Decode(&stats); err != nil {
		t.Fatalf("decode /stats error = %v", err)
	}
	return stats
}

func disconnectOneClient(t *testing.T, baseURL string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events error = %v", err)
	}
	reader := bufio.NewReader(response.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read first SSE line error = %v", err)
	}
	cancel()
	response.Body.Close()
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true before deadline")
}
