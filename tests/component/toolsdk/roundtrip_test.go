//go:build component

// Package toolsdk is the tool-sdk component smoke (spec §28.24, TOL-018): a remote
// tool server built ENTIRELY on the TypeScript Extension SDK verifies a REAL signed
// invoke and answers with an SDK-signed callback, so the whole signed round-trip
// runs through TWO SDK implementations — the Go executor signs the invoke, the TS
// SDK verifies it; the TS SDK signs the callback, the Go control-plane callback
// endpoint verifies it. It runs only under `make test-component TEST=tool-sdk`,
// which starts a throwaway Postgres (exports PALAI_COMPONENT_POSTGRES_URL) and
// provides node --experimental-strip-types. The HMAC secret is an in-process
// []byte, never logged.
//
// Honest ceiling: localhost, executor-driven, NO real model — the TS-SDK server
// variant is proven at the component tier only. The live tier (real model driving
// the tool choice, Go-SDK harness) is remote_tool_live_test.go.
package toolsdk

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/packages/coordinator"
)

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// TestToolSDKServerVariantSignedRoundtrip drives a real executor.Invoke against a
// TS-SDK tool server and asserts the SDK-signed callback one-use-completes the
// durable operation and returns the tool result.
func TestToolSDKServerVariantSignedRoundtrip(t *testing.T) {
	dbURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if dbURL == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=tool-sdk")
	}
	ctx := context.Background()

	cs, err := coordinator.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool := cs.Pool()

	org, project := newID("org"), newID("prj")
	mustExec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)

	// The shared HMAC secret (in-memory only; never logged). The org-scoped resolver
	// hands it to the callback endpoint verifier; the SAME bytes reach the TS server
	// via env, so a genuine HMAC is enforced on both directions.
	secret := []byte("tool-sdk-component-hmac-secret")
	ops := remotehttp.NewOperations(pool)
	resolver := func(o, ref string) ([]byte, error) {
		if o == org && ref == "sig-ref" {
			return secret, nil
		}
		return nil, nil
	}

	mux := http.NewServeMux()
	mux.Handle("POST /v1/tool-callbacks/{operation_id}", api.NewToolCallbackHandler(ops, resolver))
	callbackServer := httptest.NewServer(mux)
	t.Cleanup(callbackServer.Close)

	toolURL := startSDKToolServer(t, secret)

	executor := remotehttp.NewExecutor(ops, remotehttp.WithCallbackBaseURL(callbackServer.URL))
	result, err := executor.Invoke(ctx, remotehttp.Invocation{
		URL:          toolURL,
		AllowPrivate: true, // 127.0.0.1 + http self-host downgrade
		Secret:       secret,
		ToolCallID:   newID("tcall"),
		ToolRevision: "trev_component",
		RunID:        newID("run"),
		AttemptID:    newID("att"),
		RequestHash:  "sha256:component",
		Arguments:    map[string]any{"query": "weather"},
		Org:          org,
		Project:      project,
		SecretRef:    "sig-ref",
		Fence:        1,
		TimeoutMS:    15000,
	})
	if err != nil {
		t.Fatalf("executor.Invoke through the TS-SDK server: %v", err)
	}
	if got := result["answer"]; got != "sunny" {
		t.Fatalf("result = %v, want {answer: sunny} from the SDK-signed callback", result)
	}

	// The durable operation completed via the one-use SDK-signed callback (not a silent commit).
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM remote_tool_operations ORDER BY created_at DESC LIMIT 1`).Scan(&state); err != nil {
		t.Fatalf("read operation state: %v", err)
	}
	if state != "completed" {
		t.Fatalf("operation state = %q, want completed (invoke -> TS-SDK verify -> 202 -> TS-SDK callback -> completed)", state)
	}
}

// startSDKToolServer spawns the TS-SDK fixture server (node --experimental-strip-
// types) and returns its localhost base URL once it prints "LISTENING <port>". The
// secret is passed via env (in-memory), never on the command line or in a log.
func startSDKToolServer(t *testing.T, secret []byte) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source path")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	server := filepath.Join(root, "sdks", "typescript", "extension-sdk", "fixtures", "server.ts")
	if _, err := os.Stat(server); err != nil {
		t.Fatalf("SDK fixture server missing at %s: %v", server, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "node", "--experimental-strip-types", server)
	cmd.Env = append(os.Environ(), "TOOL_SDK_SECRET="+string(secret))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("node stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node SDK server: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	type result struct {
		port string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if port, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "LISTENING "); ok {
				ch <- result{port: port}
				return
			}
		}
		ch <- result{err: fmt.Errorf("node SDK server exited before it reported LISTENING")}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("%v", r.err)
		}
		return "http://127.0.0.1:" + r.port
	case <-time.After(20 * time.Second):
		t.Fatal("node SDK server did not report LISTENING within 20s")
		return ""
	}
}
