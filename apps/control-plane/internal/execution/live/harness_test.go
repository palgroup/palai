//go:build live

package live

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"

	"github.com/palgroup/palai/storage"
)

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// seedRun creates org -> project -> session -> response -> run (state=queued) so RunContext resolves
// and the orchestrator can drive the run. The input prompt requests the tool, and the project's
// config_policy puts recovery_note in the effective set so dispatchModel advertises it to the real
// provider (E12 T1) — the model then reaches the tool boundary the checkpoint smokes exercise.
func seedRun(t *testing.T, pool *pgxpool.Pool) (coordinator.Tenant, string, string, string) {
	t.Helper()
	ctx := context.Background()
	tenant := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	session, response, runID := newID("ses"), newID("resp"), newID("run")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	do(`INSERT INTO projects (id, organization_id, config_policy) VALUES ($1, $2, $3)`,
		tenant.Project, tenant.Organization, []byte(`{"default_tools":["recovery_note"]}`))
	do(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, session, tenant.Organization, tenant.Project)
	do(`INSERT INTO responses (id, organization_id, project_id, session_id, state, input)
	    VALUES ($1, $2, $3, $4, 'queued', $5)`,
		response, tenant.Organization, tenant.Project, session,
		[]byte(`"Record a short note with the recovery_note tool, then confirm you are done."`))
	do(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state)
	    VALUES ($1, $2, $3, $4, $5, 'queued')`,
		runID, tenant.Organization, tenant.Project, session, response)
	return tenant, session, response, runID
}

// lastProviderRequestID reads the newest committed model result's provider_request_id (a real
// chatcmpl-... for provider-one), the live round-trip receipt for the run.
func lastProviderRequestID(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, runID string) string {
	t.Helper()
	rows, err := pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT result FROM model_requests WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 AND state='completed' ORDER BY updated_at DESC`,
		runID, tenant.Organization, tenant.Project)
	if err != nil {
		t.Fatalf("read model results: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var result []byte
		if err := rows.Scan(&result); err != nil {
			t.Fatalf("scan model result: %v", err)
		}
		var body struct {
			ProviderRequestID string `json:"provider_request_id"`
		}
		_ = json.Unmarshal(result, &body)
		if body.ProviderRequestID != "" {
			return body.ProviderRequestID
		}
	}
	return ""
}

// latestRecoveryLevel reads the newest attempt.recovering.v1 level for the session.
func latestRecoveryLevel(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, sessionID string) string {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT payload FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type='attempt.recovering.v1' ORDER BY seq DESC LIMIT 1`,
		sessionID, tenant.Organization, tenant.Project).Scan(&payload); err != nil {
		t.Fatalf("read attempt.recovering.v1: %v", err)
	}
	var body struct {
		Level string `json:"level"`
	}
	_ = json.Unmarshal(payload, &body)
	return body.Level
}

// recoveryProof reads the newest recovery.proof.v1 into a RecoveryProof for its completeness check.
func recoveryProof(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, sessionID string) recovery.RecoveryProof {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT payload FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type='recovery.proof.v1' ORDER BY seq DESC LIMIT 1`,
		sessionID, tenant.Organization, tenant.Project).Scan(&payload); err != nil {
		t.Fatalf("read recovery.proof.v1: %v", err)
	}
	var proof recovery.RecoveryProof
	if err := json.Unmarshal(payload, &proof); err != nil {
		t.Fatalf("decode RecoveryProof: %v", err)
	}
	return proof
}

// subprocessDialer runs the reference engine as a bare uv-subprocess over stdio. killAfterCheckpoint
// SIGKILLs the process on the first Receive AFTER a checkpoint.offer has been delivered to (and
// persisted by) the orchestrator — the post-persist boundary kill the smoke exercises.
type subprocessDialer struct {
	engineDir           string
	killAfterCheckpoint bool
}

func (d *subprocessDialer) Dial(_ context.Context, attempt execution.AttemptDescriptor) (execution.EngineChannel, error) {
	cmd := exec.Command("uv", "run", "--locked", "--project", d.engineDir, "python", "-m", "palai_engine")
	cmd.Env = []string{
		"PALAI_RUN_ID=" + string(attempt.RunID),
		"PALAI_ATTEMPT_ID=" + string(attempt.AttemptID),
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PYTHONPATH=" + filepath.Join(d.engineDir, "src"),
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := &subprocessChannel{cmd: cmd, stdin: stdin, scanner: bufio.NewScanner(stdout), stderr: &stderr, killAfterCheckpoint: d.killAfterCheckpoint}
	// The engine handshakes supervisor.hello -> engine.ready before it reads any run input
	// (engine __main__.require_hello). The controller sends the hello, exactly as the e2e channel
	// does; without it the engine blocks on its first read and the orchestrator times out on
	// engine.ready.
	if err := ch.Send(context.Background(), helloFrame(attempt)); err != nil {
		return nil, err
	}
	return ch, nil
}

// helloFrame is the supervisor.hello the controller sends to open the engine handshake.
func helloFrame(attempt execution.AttemptDescriptor) contracts.EngineFrame {
	return contracts.EngineFrame{
		Protocol:  "engine.v1",
		ID:        contracts.FrameID(newID("frm")),
		Type:      "supervisor.hello",
		Sequence:  1,
		Time:      time.Now().UTC().Format(time.RFC3339),
		RunID:     attempt.RunID,
		AttemptID: attempt.AttemptID,
		Data:      map[string]any{},
	}
}

type subprocessChannel struct {
	cmd                 *exec.Cmd
	stdin               io.WriteCloser
	scanner             *bufio.Scanner
	stderr              *bytes.Buffer
	killAfterCheckpoint bool
	sawCheckpoint       bool
}

func (c *subprocessChannel) Send(_ context.Context, frame contracts.EngineFrame) error {
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write engine stdin: %w", err)
	}
	return nil
}

func (c *subprocessChannel) Receive(ctx context.Context) (contracts.EngineFrame, error) {
	// Post-persist boundary kill: the orchestrator persisted the checkpoint.offer before this next
	// Receive, so killing here leaves a durable checkpoint and a dead attempt for the ladder to restore.
	if c.killAfterCheckpoint && c.sawCheckpoint {
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		return contracts.EngineFrame{}, fmt.Errorf("engine SIGKILLed after checkpoint persist")
	}
	type scanned struct {
		frame contracts.EngineFrame
		err   error
	}
	done := make(chan scanned, 1)
	go func() { f, err := c.scan(); done <- scanned{f, err} }()
	select {
	case <-ctx.Done():
		return contracts.EngineFrame{}, ctx.Err()
	case r := <-done:
		if r.err == nil && r.frame.Type == "checkpoint.offer" {
			c.sawCheckpoint = true
		}
		return r.frame, r.err
	}
}

func (c *subprocessChannel) scan() (contracts.EngineFrame, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return contracts.EngineFrame{}, err
		}
		if c.stderr.Len() > 0 {
			return contracts.EngineFrame{}, fmt.Errorf("%w: engine stderr: %s", io.EOF, c.stderr.String())
		}
		return contracts.EngineFrame{}, io.EOF
	}
	var frame contracts.EngineFrame
	if err := json.Unmarshal(c.scanner.Bytes(), &frame); err != nil {
		return contracts.EngineFrame{}, fmt.Errorf("decode engine frame: %w", err)
	}
	return frame, nil
}

func (c *subprocessChannel) Close() error {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}
