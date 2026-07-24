// Command palai-capability-worker is the macOS FIXTURE capability worker (E17 Task 9, spec §31, WRK-001..007).
// It is a HOST-NATIVE process — run OUTSIDE any container, so it is genuinely separated from the control
// plane's network (a real private-network posture) — that OUTBOUND-ENROLLS to the control-plane capability
// gateway with a one-time token, receives a short-lived workload identity, then polls for and runs ONE typed
// operation: swift.build-check. It opens NO inbound port (it only dials the gateway).
//
// The worker's own no-tunnel stance (defence in depth on top of the gateway/store gate): it knows how to run
// EXACTLY swift.build-check and REFUSES any other operation — it is not a general executor. It uses swiftc if
// the host has it (a REAL type-check) and an honest toy check otherwise. If the job names a secret handle it
// REDEEMS it, uses it internally, and NEVER echoes the value into the receipt, the output artifact, or its
// logs.
//
// HONEST CEILING: this is NOT a macOS/iOS build. swift.build-check is a type-check only — no codegen, no
// linking, no signing. There is no signing cert / provisioning profile / store credential anywhere. A real
// signed Apple build is a separate capability (apple-build, disabled) and the §6 leg 3 operator work.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/workers"
)

func main() {
	gateway := flag.String("gateway", "", "control-plane capability gateway base URL (outbound)")
	enrollToken := flag.String("enroll-token", "", "one-time enrollment token (spent on enroll)")
	pollFor := flag.Duration("poll-for", 15*time.Second, "how long to poll for a job before giving up")
	flag.Parse()

	if *gateway == "" || *enrollToken == "" {
		fatal("both -gateway and -enroll-token are required")
	}
	// Read the enrollment token from the env when it is passed as a sentinel, so it never lands in argv/ps.
	if *enrollToken == "$PALAI_WORKER_ENROLL_TOKEN" {
		*enrollToken = os.Getenv("PALAI_WORKER_ENROLL_TOKEN")
	}

	c := &client{base: *gateway, http: &http.Client{Timeout: 90 * time.Second}}

	// 1. Enroll outbound. The spec RUNTIME-detects the host toolchain digest — never a signing key.
	if err := c.enroll(*enrollToken); err != nil {
		fatal("enroll: %v", err)
	}
	fmt.Printf("worker enrolled worker_id=%s os=%s arch=%s toolchain=%s\n", c.workerID, runtime.GOOS, runtime.GOARCH, toolchainMode())

	// 2. Poll for ONE job and run it.
	deadline := time.Now().Add(*pollFor)
	for time.Now().Before(deadline) {
		job, got, err := c.claim()
		if err != nil {
			fatal("claim: %v", err)
		}
		if !got {
			time.Sleep(150 * time.Millisecond)
			continue
		}
		if err := c.run(job); err != nil {
			fatal("run job %s: %v", job.JobID, err)
		}
		fmt.Printf("worker completed job_id=%s operation=%s\n", job.JobID, job.Operation)
		return
	}
	fatal("no job within %s", *pollFor)
}

type client struct {
	base     string
	http     *http.Client
	workerID string
	workload string
}

type claimBody struct {
	JobID            string   `json:"job_id"`
	Operation        string   `json:"operation"`
	InputArtifact    string   `json:"input_artifact_b64"`
	SecretHandleRefs []string `json:"secret_handle_refs"`
	DeadlineUnix     int64    `json:"deadline_unix"`
}

func (c *client) enroll(token string) error {
	digests := map[string]string{}
	if path, err := exec.LookPath("swiftc"); err == nil {
		digests["swiftc"] = "present:" + path // a presence marker, never a key
	}
	body, _ := json.Marshal(map[string]any{
		"capability_version": "0.1.0", "os": runtime.GOOS, "arch": runtime.GOARCH,
		"toolchain_digests": digests, "capacity": 1,
	})
	resp, err := c.post("/capability/enroll", token, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	var out struct {
		WorkerID      string `json:"worker_id"`
		WorkloadToken string `json:"workload_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	c.workerID, c.workload = out.WorkerID, out.WorkloadToken
	return nil
}

func (c *client) claim() (claimBody, bool, error) {
	resp, err := c.post("/capability/claim", c.workload, nil)
	if err != nil {
		return claimBody{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return claimBody{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return claimBody{}, false, fmt.Errorf("status %s", resp.Status)
	}
	var job claimBody
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return claimBody{}, false, err
	}
	return job, true, nil
}

// run executes ONE typed operation. It refuses anything but swift.build-check — the worker is not a general
// executor (no tunnel).
func (c *client) run(job claimBody) error {
	if job.Operation != "swift.build-check" {
		// A defensive worker-side no-tunnel refusal: report failure, do NOT execute an unknown operation.
		return c.submit(job.JobID, "failed", job.Operation, map[string]any{"error": "unsupported operation (worker runs only swift.build-check)"}, nil)
	}

	source, err := base64.StdEncoding.DecodeString(job.InputArtifact)
	if err != nil {
		return fmt.Errorf("decode input: %w", err)
	}

	// If the job names a secret handle, redeem it, "use" it, and NEVER echo the value. Here the use is a
	// no-op marker in the receipt confirming a handle was redeemed WITHOUT revealing it.
	redeemed := false
	for _, name := range job.SecretHandleRefs {
		if _, err := c.redeem(job.JobID, name); err == nil {
			redeemed = true
		}
	}

	res, report := workers.BuildCheck(context.Background(), source)
	receipt := map[string]any{
		"mode":            res.Mode,
		"ok":              res.OK,
		"artifact_sha256": res.ArtifactSHA256,
		"secret_redeemed": redeemed, // whether a handle was used — NEVER the value
	}
	class := "completed"
	if !res.OK {
		class = "failed"
	}
	return c.submit(job.JobID, class, job.Operation, receipt, report)
}

func (c *client) redeem(jobID, name string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"job_id": jobID, "handle_name": name})
	resp, err := c.post("/capability/redeem", c.workload, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	var out struct {
		ValueB64 string `json:"value_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.ValueB64)
}

func (c *client) submit(jobID, class, operation string, receipt map[string]any, output []byte) error {
	payload := map[string]any{"job_id": jobID, "class": class, "operation": operation, "receipt": receipt}
	if len(output) > 0 {
		payload["output_artifact_b64"] = base64.StdEncoding.EncodeToString(output)
	}
	body, _ := json.Marshal(payload)
	resp, err := c.post("/capability/result", c.workload, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("result rejected: %s %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

func (c *client) post(path, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return c.http.Do(req)
}

func toolchainMode() string {
	if _, err := exec.LookPath("swiftc"); err == nil {
		return "real-swiftc"
	}
	return "toy"
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "capability-worker: "+format+"\n", args...)
	os.Exit(1)
}
