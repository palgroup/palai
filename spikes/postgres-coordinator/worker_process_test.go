package coordinator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

type workerRequest struct {
	Action     string
	JobID      string
	Owner      string
	Fence      int64
	Lease      time.Duration
	ResultHash string
}

type workerReceipt struct {
	Kind string `json:"kind"`
	Claim
}

type workerProcess struct {
	command *exec.Cmd
	decoder *json.Decoder
	stderr  *synchronizedBuffer
	mu      sync.Mutex
	waited  bool
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func TestWorkerProcess(t *testing.T) {
	if os.Getenv("PALAI_SPIKE_WORKER_HELPER") != "1" {
		return
	}
	url := os.Getenv("PALAI_SPIKE_POSTGRES_URL")
	store, err := NewStore(t.Context(), url)
	if err != nil {
		workerFatal("open store", err)
	}
	defer store.Close()
	action := os.Getenv("PALAI_SPIKE_WORKER_ACTION")
	jobID := os.Getenv("PALAI_SPIKE_JOB_ID")
	owner := os.Getenv("PALAI_SPIKE_OWNER")
	switch action {
	case "claim-hold", "claim-once":
		leaseMilliseconds, err := strconv.ParseInt(os.Getenv("PALAI_SPIKE_LEASE_MS"), 10, 64)
		if err != nil || leaseMilliseconds < 1 {
			workerFatal("parse lease", err)
		}
		claim, err := store.Claim(t.Context(), jobID, owner, time.Duration(leaseMilliseconds)*time.Millisecond)
		if err != nil {
			workerFatal("claim", err)
		}
		writeWorkerReceipt(workerReceipt{Kind: "claim", Claim: claim})
		if action == "claim-once" {
			store.Close()
			os.Exit(0)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "complete-hold":
		fence, err := strconv.ParseInt(os.Getenv("PALAI_SPIKE_FENCE"), 10, 64)
		if err != nil || fence < 1 {
			workerFatal("parse fence", err)
		}
		claim := Claim{JobID: jobID, Owner: owner, Fence: fence}
		err = RunHeldCompletionWorker(t.Context(), store, claim, os.Getenv("PALAI_SPIKE_RESULT_HASH"), func() error {
			writeWorkerReceipt(workerReceipt{Kind: "completion-staged", Claim: claim})
			return nil
		})
		workerFatal("held completion returned", err)
	default:
		workerFatal("dispatch", fmt.Errorf("unknown action %q", action))
	}
}

func startWorkerProcess(t *testing.T, request workerRequest) *workerProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	command := exec.Command(executable, "-test.run=^TestWorkerProcess$")
	command.Env = append(os.Environ(),
		"PALAI_SPIKE_WORKER_HELPER=1",
		"PALAI_SPIKE_WORKER_ACTION="+request.Action,
		"PALAI_SPIKE_JOB_ID="+request.JobID,
		"PALAI_SPIKE_OWNER="+request.Owner,
		"PALAI_SPIKE_FENCE="+strconv.FormatInt(request.Fence, 10),
		"PALAI_SPIKE_LEASE_MS="+strconv.FormatInt(request.Lease.Milliseconds(), 10),
		"PALAI_SPIKE_RESULT_HASH="+request.ResultHash,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	stderr := &synchronizedBuffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatalf("worker Start() error = %v", err)
	}
	process := &workerProcess{command: command, decoder: json.NewDecoder(stdout), stderr: stderr}
	t.Cleanup(func() { process.cleanup() })
	return process
}

func (process *workerProcess) readReceipt(t *testing.T) workerReceipt {
	t.Helper()
	type result struct {
		receipt workerReceipt
		err     error
	}
	resultChannel := make(chan result, 1)
	go func() {
		var receipt workerReceipt
		err := process.decoder.Decode(&receipt)
		resultChannel <- result{receipt: receipt, err: err}
	}()
	select {
	case result := <-resultChannel:
		if result.err != nil {
			t.Fatalf("decode worker receipt error = %v; stderr=%s", result.err, process.stderr.String())
		}
		return result.receipt
	case <-time.After(3 * time.Second):
		t.Fatalf("worker receipt timeout; stderr=%s", process.stderr.String())
		return workerReceipt{}
	}
}

func (process *workerProcess) kill(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(process.command.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL worker error = %v", err)
	}
	err := process.wait()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ProcessState.Success() {
		t.Fatalf("killed worker Wait() error = %v; stderr=%s", err, process.stderr.String())
	}
}

func (process *workerProcess) waitSuccess(t *testing.T) {
	t.Helper()
	if err := process.wait(); err != nil {
		t.Fatalf("worker Wait() error = %v; stderr=%s", err, process.stderr.String())
	}
}

func (process *workerProcess) wait() error {
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.waited {
		return nil
	}
	process.waited = true
	return process.command.Wait()
}

func (process *workerProcess) cleanup() {
	process.mu.Lock()
	if process.waited {
		process.mu.Unlock()
		return
	}
	process.waited = true
	process.mu.Unlock()
	_ = process.command.Process.Kill()
	_ = process.command.Wait()
}

func writeWorkerReceipt(receipt workerReceipt) {
	if err := json.NewEncoder(os.Stdout).Encode(receipt); err != nil {
		workerFatal("write receipt", err)
	}
}

func workerFatal(action string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
	os.Exit(2)
}
