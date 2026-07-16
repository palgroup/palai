package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type candidateSpec struct {
	Name    string
	Command string
	Args    []string
}

type readyMessage struct {
	Event   string `json:"event"`
	Address string `json:"address"`
	Runtime string `json:"runtime"`
}

type candidateProcess struct {
	BaseURL       string
	ReadyDuration time.Duration
	command       *exec.Cmd
	done          chan struct{}
	stdoutDone    chan struct{}
	stdout        *lockedBuffer
	stderr        *lockedBuffer
	mu            sync.Mutex
	waitErr       error
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

type loadProfile struct {
	Connections      int
	Reconnects       int
	RestartCycles    int
	ConnectDeadline  time.Duration
	ShutdownDeadline time.Duration
	Heartbeat        time.Duration
	MaximumErrors    int
}

type candidateResult struct {
	Name               string
	ReadyDuration      time.Duration
	IdleRSSBytes       int64
	ConnectedRSSBytes  int64
	Connected          int
	Reconnects         int
	RestartCycles      int
	SequenceDuplicates int
	SequenceGaps       int
	Errors             int
	CancelRequests     int64
	ShutdownDuration   time.Duration
}

type serverStats struct {
	ActiveConnections int64 `json:"active_connections"`
	Disconnects       int64 `json:"disconnects"`
	CancelRequests    int64 `json:"cancel_requests"`
}

type fixtureConnection struct {
	body   io.ReadCloser
	cancel context.CancelFunc
}

type connectionResult struct {
	connection *fixtureConnection
	duplicates int
	gaps       int
	err        error
}

func repositoryRoot() (string, error) {
	output, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("locate repository root: %w", err)
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", errors.New("locate repository root: empty path")
	}
	return root, nil
}

func prepareCandidates(ctx context.Context, root string) ([]candidateSpec, error) {
	buildDirectory := filepath.Join(root, "spikes", "control-plane", ".build")
	if err := os.MkdirAll(buildDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("create build directory: %w", err)
	}
	goBinary := filepath.Join(buildDirectory, "go-server")
	if err := runBuild(ctx, root, "go", "build", "-trimpath", "-o", goBinary, "./spikes/control-plane/go-server"); err != nil {
		return nil, err
	}
	nodeDirectory := filepath.Join(root, "spikes", "control-plane", "node-server")
	if err := runBuild(ctx, root, "pnpm", "--dir", nodeDirectory, "run", "build"); err != nil {
		return nil, err
	}
	nodeBinary, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("locate node: %w", err)
	}
	nodeServer := filepath.Join(nodeDirectory, ".build", "server.js")
	if _, err := os.Stat(nodeServer); err != nil {
		return nil, fmt.Errorf("locate compiled Node candidate: %w", err)
	}
	return []candidateSpec{
		{Name: "go", Command: goBinary},
		{Name: "node", Command: nodeBinary, Args: []string{nodeServer}},
	}, nil
}

func runBuild(ctx context.Context, directory, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s candidate: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func startCandidate(ctx context.Context, candidate candidateSpec, heartbeat time.Duration) (*candidateProcess, error) {
	if heartbeat <= 0 {
		return nil, errors.New("start candidate: heartbeat must be positive")
	}
	started := time.Now()
	args := append([]string(nil), candidate.Args...)
	args = append(args, "--port=0", "--heartbeat="+heartbeat.String())
	command := exec.CommandContext(ctx, candidate.Command, args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("start %s candidate stdout: %w", candidate.Name, err)
	}
	stderr := &lockedBuffer{}
	stdoutRemainder := &lockedBuffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start %s candidate: %w", candidate.Name, err)
	}
	process := &candidateProcess{
		command:    command,
		done:       make(chan struct{}),
		stdoutDone: make(chan struct{}),
		stdout:     stdoutRemainder,
		stderr:     stderr,
	}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.waitErr = err
		process.mu.Unlock()
		close(process.done)
	}()

	readyChannel := make(chan readyMessage, 1)
	readError := make(chan error, 1)
	go func() {
		defer close(process.stdoutDone)
		reader := bufio.NewReader(stdout)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			readError <- fmt.Errorf("read readiness: %w", err)
			return
		}
		var ready readyMessage
		if err := json.Unmarshal(line, &ready); err != nil {
			readError <- fmt.Errorf("decode readiness: %w", err)
			return
		}
		readyChannel <- ready
		_, _ = io.Copy(stdoutRemainder, reader)
	}()

	readinessTimer := time.NewTimer(5 * time.Second)
	defer readinessTimer.Stop()
	var ready readyMessage
	select {
	case ready = <-readyChannel:
	case err := <-readError:
		process.forceStop()
		return nil, fmt.Errorf("start %s candidate: %w; stderr=%s", candidate.Name, err, strings.TrimSpace(stderr.String()))
	case <-process.done:
		return nil, fmt.Errorf("start %s candidate: exited before readiness; stderr=%s", candidate.Name, strings.TrimSpace(stderr.String()))
	case <-readinessTimer.C:
		process.forceStop()
		return nil, fmt.Errorf("start %s candidate: readiness timeout; stderr=%s", candidate.Name, strings.TrimSpace(stderr.String()))
	case <-ctx.Done():
		process.forceStop()
		return nil, fmt.Errorf("start %s candidate: %w", candidate.Name, ctx.Err())
	}
	if ready.Event != "ready" || ready.Runtime != candidate.Name {
		process.forceStop()
		return nil, fmt.Errorf("start %s candidate: invalid readiness %+v", candidate.Name, ready)
	}
	host, port, err := net.SplitHostPort(ready.Address)
	if err != nil || host != "127.0.0.1" || port == "0" {
		process.forceStop()
		return nil, fmt.Errorf("start %s candidate: invalid listener address %q", candidate.Name, ready.Address)
	}
	process.BaseURL = "http://" + ready.Address
	process.ReadyDuration = time.Since(started)
	return process, nil
}

func (process *candidateProcess) stop(ctx context.Context) error {
	select {
	case <-process.done:
		return process.result()
	default:
	}
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal candidate: %w", err)
	}
	select {
	case <-process.done:
		return process.result()
	case <-ctx.Done():
		_ = process.command.Process.Kill()
		<-process.done
		return fmt.Errorf("candidate shutdown: %w; stderr=%s", ctx.Err(), strings.TrimSpace(process.stderr.String()))
	}
}

func (process *candidateProcess) forceStop() {
	select {
	case <-process.done:
		return
	default:
	}
	_ = process.command.Process.Kill()
	<-process.done
}

func (process *candidateProcess) result() error {
	<-process.stdoutDone
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.waitErr != nil {
		return fmt.Errorf("candidate exited: %w; stderr=%s", process.waitErr, strings.TrimSpace(process.stderr.String()))
	}
	if extra := strings.TrimSpace(process.stdout.String()); extra != "" {
		return fmt.Errorf("candidate wrote unexpected stdout after readiness: %q", extra)
	}
	return nil
}

func quickLoadProfile() loadProfile {
	return loadProfile{
		Connections:      25,
		Reconnects:       10,
		RestartCycles:    1,
		ConnectDeadline:  10 * time.Second,
		ShutdownDeadline: 5 * time.Second,
		Heartbeat:        250 * time.Millisecond,
		MaximumErrors:    0,
	}
}

func evidenceLoadProfile() loadProfile {
	return loadProfile{
		Connections:      1000,
		Reconnects:       100,
		RestartCycles:    1,
		ConnectDeadline:  30 * time.Second,
		ShutdownDeadline: 5 * time.Second,
		Heartbeat:        15 * time.Second,
		MaximumErrors:    0,
	}
}

func runCandidateLoad(ctx context.Context, candidate candidateSpec, profile loadProfile) (candidateResult, error) {
	result := candidateResult{Name: candidate.Name}
	process, err := startCandidate(ctx, candidate, profile.Heartbeat)
	if err != nil {
		return result, err
	}
	defer process.forceStop()
	result.ReadyDuration = process.ReadyDuration
	result.IdleRSSBytes, err = processRSS(process.command.Process.Pid)
	if err != nil {
		return result, fmt.Errorf("measure %s idle RSS: %w", candidate.Name, err)
	}

	transport := &http.Transport{
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        profile.Connections + profile.Reconnects + 10,
		MaxIdleConnsPerHost: profile.Connections + profile.Reconnects + 10,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	connectionContext, cancelConnections := context.WithCancel(ctx)
	defer cancelConnections()
	connections, duplicates, gaps, err := openConnections(
		connectionContext,
		client,
		process.BaseURL,
		profile.Connections,
		profile.ConnectDeadline,
	)
	if err != nil {
		closeConnections(connections)
		return result, fmt.Errorf("connect %s clients: %w", candidate.Name, err)
	}
	defer func() { closeConnections(connections) }()
	result.Connected = len(connections)
	result.SequenceDuplicates += duplicates
	result.SequenceGaps += gaps

	if err := waitForActiveConnections(ctx, client, process.BaseURL, int64(profile.Connections), 2*time.Second); err != nil {
		return result, err
	}
	result.ConnectedRSSBytes, err = processRSS(process.command.Process.Pid)
	if err != nil {
		return result, fmt.Errorf("measure %s connected RSS: %w", candidate.Name, err)
	}

	reconnects, duplicates, gaps, err := runReconnects(connectionContext, client, process.BaseURL, profile.Reconnects, profile.ConnectDeadline)
	if err != nil {
		return result, fmt.Errorf("reconnect %s clients: %w", candidate.Name, err)
	}
	result.Reconnects = reconnects
	result.SequenceDuplicates += duplicates
	result.SequenceGaps += gaps
	stats, err := waitForStats(ctx, client, process.BaseURL, 2*time.Second, func(stats serverStats) bool {
		return stats.ActiveConnections == int64(profile.Connections) && stats.Disconnects >= int64(profile.Reconnects)
	})
	if err != nil {
		return result, err
	}
	result.CancelRequests = stats.CancelRequests

	shutdownContext, cancelShutdown := context.WithTimeout(ctx, profile.ShutdownDeadline)
	shutdownStarted := time.Now()
	err = process.stop(shutdownContext)
	result.ShutdownDuration = time.Since(shutdownStarted)
	cancelShutdown()
	if err != nil {
		return result, err
	}
	cancelConnections()
	closeConnections(connections)
	connections = nil

	for cycle := 0; cycle < profile.RestartCycles; cycle++ {
		restarted, err := startCandidate(ctx, candidate, profile.Heartbeat)
		if err != nil {
			return result, fmt.Errorf("restart %s candidate: %w", candidate.Name, err)
		}
		if err := requireHealth(ctx, restarted.BaseURL); err != nil {
			restarted.forceStop()
			return result, fmt.Errorf("restart %s health: %w", candidate.Name, err)
		}
		cycleContext, cancelCycle := context.WithTimeout(ctx, profile.ShutdownDeadline)
		cycleStarted := time.Now()
		err = restarted.stop(cycleContext)
		cycleDuration := time.Since(cycleStarted)
		cancelCycle()
		if cycleDuration > result.ShutdownDuration {
			result.ShutdownDuration = cycleDuration
		}
		if err != nil {
			restarted.forceStop()
			return result, fmt.Errorf("stop restarted %s candidate: %w", candidate.Name, err)
		}
		result.RestartCycles++
	}
	return result, nil
}

func openConnections(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	count int,
	deadline time.Duration,
) ([]*fixtureConnection, int, int, error) {
	operationContext, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	results := make(chan connectionResult, count)
	for index := 0; index < count; index++ {
		go func() {
			connection, duplicates, gaps, err := openFixtureConnection(ctx, client, baseURL, "", []int{1, 2})
			results <- connectionResult{connection: connection, duplicates: duplicates, gaps: gaps, err: err}
		}()
	}
	connections := make([]*fixtureConnection, 0, count)
	duplicates := 0
	gaps := 0
	for len(connections) < count {
		select {
		case result := <-results:
			if result.err != nil {
				closeConnections(connections)
				return nil, duplicates, gaps, result.err
			}
			connections = append(connections, result.connection)
			duplicates += result.duplicates
			gaps += result.gaps
		case <-operationContext.Done():
			closeConnections(connections)
			return nil, duplicates, gaps, operationContext.Err()
		}
	}
	return connections, duplicates, gaps, nil
}

func runReconnects(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	count int,
	deadline time.Duration,
) (int, int, int, error) {
	operationContext, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	results := make(chan connectionResult, count)
	for index := 0; index < count; index++ {
		go func() {
			connection, duplicates, gaps, err := openFixtureConnection(ctx, client, baseURL, "1", []int{2})
			if connection != nil {
				connection.cancel()
				_ = connection.body.Close()
			}
			results <- connectionResult{duplicates: duplicates, gaps: gaps, err: err}
		}()
	}
	completed := 0
	duplicates := 0
	gaps := 0
	for completed < count {
		select {
		case result := <-results:
			if result.err != nil {
				return completed, duplicates, gaps, result.err
			}
			completed++
			duplicates += result.duplicates
			gaps += result.gaps
		case <-operationContext.Done():
			return completed, duplicates, gaps, operationContext.Err()
		}
	}
	return completed, duplicates, gaps, nil
}

func openFixtureConnection(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	lastEventID string,
	wantSequences []int,
) (*fixtureConnection, int, int, error) {
	requestContext, cancel := context.WithCancel(ctx)
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, baseURL+"/events", nil)
	if err != nil {
		cancel()
		return nil, 0, 0, err
	}
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		return nil, 0, 0, err
	}
	if response.StatusCode != http.StatusOK {
		cancel()
		response.Body.Close()
		return nil, 0, 0, fmt.Errorf("events status %d", response.StatusCode)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		response.Body.Close()
		return nil, 0, 0, fmt.Errorf("events content type %q", response.Header.Get("Content-Type"))
	}
	sequences, err := readFixtureSequences(bufio.NewReader(response.Body), len(wantSequences))
	if err != nil {
		cancel()
		response.Body.Close()
		return nil, 0, 0, err
	}
	duplicates, gaps := compareSequences(sequences, wantSequences)
	return &fixtureConnection{body: response.Body, cancel: cancel}, duplicates, gaps, nil
}

func readFixtureSequences(reader *bufio.Reader, count int) ([]int, error) {
	sequences := make([]int, 0, count)
	currentID := ""
	currentEvent := ""
	currentData := ""
	for len(sequences) < count {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case line == "":
			if currentID == "" && currentEvent == "" && currentData == "" {
				continue
			}
			if currentEvent != "fixture" {
				return nil, fmt.Errorf("unexpected event type %q", currentEvent)
			}
			sequence, err := strconv.Atoi(currentID)
			if err != nil {
				return nil, fmt.Errorf("invalid event ID %q", currentID)
			}
			wantData := fmt.Sprintf(`{"sequence":%d}`, sequence)
			if currentData != wantData {
				return nil, fmt.Errorf("event %d data = %q, want %q", sequence, currentData, wantData)
			}
			sequences = append(sequences, sequence)
			currentID, currentEvent, currentData = "", "", ""
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "id:"):
			currentID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			currentData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		default:
			return nil, fmt.Errorf("unexpected SSE line %q", line)
		}
	}
	return sequences, nil
}

func compareSequences(got, want []int) (duplicates int, gaps int) {
	seen := make(map[int]struct{}, len(got))
	for index, sequence := range got {
		if _, exists := seen[sequence]; exists {
			duplicates++
		}
		seen[sequence] = struct{}{}
		if index >= len(want) || sequence != want[index] {
			gaps++
		}
	}
	if len(got) < len(want) {
		gaps += len(want) - len(got)
	}
	return duplicates, gaps
}

func closeConnections(connections []*fixtureConnection) {
	for _, connection := range connections {
		if connection == nil {
			continue
		}
		connection.cancel()
		_ = connection.body.Close()
	}
}

func waitForActiveConnections(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	want int64,
	timeout time.Duration,
) error {
	_, err := waitForStats(ctx, client, baseURL, timeout, func(stats serverStats) bool {
		return stats.ActiveConnections == want
	})
	return err
}

func waitForStats(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	timeout time.Duration,
	condition func(serverStats) bool,
) (serverStats, error) {
	deadline := time.Now().Add(timeout)
	var last serverStats
	for time.Now().Before(deadline) {
		stats, err := getStats(ctx, client, baseURL)
		if err == nil {
			last = stats
			if condition(stats) {
				return stats, nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return last, fmt.Errorf("stats condition not met before deadline: %+v", last)
}

func getStats(ctx context.Context, client *http.Client, baseURL string) (serverStats, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/stats", nil)
	if err != nil {
		return serverStats{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return serverStats{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return serverStats{}, fmt.Errorf("stats status %d", response.StatusCode)
	}
	var stats serverStats
	if err := json.NewDecoder(response.Body).Decode(&stats); err != nil {
		return serverStats{}, err
	}
	return stats, nil
}

func requireHealth(ctx context.Context, baseURL string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health status %d", response.StatusCode)
	}
	return nil
}

func processRSS(pid int) (int64, error) {
	output, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	kibibytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse RSS %q: %w", strings.TrimSpace(string(output)), err)
	}
	return kibibytes * 1024, nil
}
