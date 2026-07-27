//go:build e2e

package stack

import (
	"context"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestASecretSurvivesARestartOnAStackWithNoSlackApp is the §3.6 D5 component-real proof, and it is the
// test the E21 plan names word for word: an .env.local with NO Slack credentials, the stack comes up, a
// secret is written, the stack is restarted, and the secret RESOLVES.
//
// It was RED before this task and it was red for a reason worth stating precisely, because the symptom
// the owner met — "secrets do not survive a restart" — was not what was happening. Secrets are
// envelope-encrypted rows in Postgres and identity/secrets.go re-reads the row on every resolve; there
// is no cache to lose. What the stack lost was the POINTER: compose interpolated
// PALAI_SECRET_MASTER_KEY_FILE from the invoking shell, only `palai up`'s applySlackEnv ever exported
// it, and it did so only when Slack credentials were present. Without them dbSecretStore was nil, the
// /v1/secret-refs routes were never mounted, and step 2 below answered 404 — the write never happened
// at all, on either side of the restart.
//
// Slack is deliberately absent from this environment. The whole point is that the secret store is not a
// Slack feature.
//
// Tagged e2e, like TestLiveProofFailsAgainstARealFakeStack: it brings up and tears down its own
// isolated four-service stack, so a plain `go test` does not pretend to have run it.
func TestASecretSurvivesARestartOnAStackWithNoSlackApp(t *testing.T) {
	root := repoRoot(t)
	t.Chdir(root)
	t.Setenv("PALAI_HOME", t.TempDir())
	t.Setenv("PALAI_COMPOSE_FILE", filepath.Join(root, "deploy", "compose", "compose.yaml"))
	t.Setenv("PALAI_DISPATCH_WORKERS", "1")
	t.Setenv("PALAI_MODEL_PROVIDER", "fake")
	// The three Slack variables `palai up` used to key the secret store off. Explicitly cleared so a
	// developer's exported shell cannot make this test pass for the wrong reason.
	for _, v := range []string{"SLACK_TEAM_ID", "SLACK_SIGNING_SECRET", "SLACK_BOT_TOKEN", "PALAI_SECRET_MASTER_KEY_FILE"} {
		t.Setenv(v, "")
	}

	if err := Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := Up(); err != nil {
		t.Fatalf("local up: %v", err)
	}
	t.Cleanup(func() {
		if err := Reset(true); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})

	cfg, p, err := loadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		t.Fatalf("read api key: %v", err)
	}
	api := &apiClient{baseURL: cfg.BaseURL, key: key, http: &http.Client{Timeout: 30 * time.Second}}

	// 1. The bring-up left a key the control-plane could parse. Without this the stack would not be up
	//    at all (a set-but-unparseable key file is log.Fatalf), so assert the shape, never the bytes.
	master, err := readTrimmed(p.masterKey)
	if err != nil {
		t.Fatalf("read the master key slot: %v", err)
	}
	if len(master) != 64 {
		t.Fatalf("the master-key slot holds %d bytes, want a 64-char hex key minted by the bring-up", len(master))
	}

	// 2. Write a secret. A 404 here is the whole bug: the routes are mounted only when dbSecretStore is
	//    non-nil, which is only when the master-key POINTER reached the container.
	const canary = "e21-t2-canary"
	if err := api.putSecretRef(canary, "value-that-must-outlive-a-restart"); err != nil {
		t.Fatalf("store a secret on a Slack-free stack: %v", err)
	}

	// 3. Restart the control-plane. A running one never re-reads its environment, so this is where a
	//    pointer that came from the invoking shell rather than the compose file goes missing.
	if err := recreateControlPlane(cfg, p); err != nil {
		t.Fatalf("restart the control-plane: %v", err)
	}

	// 4. The store is still mounted after the restart — the write path proves it end to end, since a
	//    nil dbSecretStore leaves the route absent rather than failing.
	if err := api.putSecretRef(canary+"-after", "written after the restart"); err != nil {
		t.Fatalf("the secret store did not survive the restart: %v", err)
	}

	// 5. And the FIRST secret still decrypts under the on-disk master key — the honest reading of
	//    "resolves". Same primitives `palai restore verify`'s canary uses; a re-minted or lost key
	//    fails here even though the row survived, which is the silently-dead install this guards.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctHex, err := pgQueryScalar(ctx, cfg.containerName("postgres"),
		"SELECT encode(ciphertext, 'hex') FROM secret_refs WHERE name = '"+canary+"'")
	if err != nil {
		t.Fatalf("read the stored ciphertext: %v", err)
	}
	if strings.TrimSpace(ctHex) == "" {
		t.Fatal("the secret written before the restart has no row after it")
	}
	sealed, err := hex.DecodeString(ctHex)
	if err != nil {
		t.Fatalf("decode the stored ciphertext: %v", err)
	}
	masterBytes, err := parseMasterKeyHex(master)
	if err != nil {
		t.Fatalf("parse the master key: %v", err)
	}
	plain, err := openSealed(masterBytes, sealed)
	if err != nil {
		t.Fatalf("a secret written before the restart does NOT decrypt under the stack's master key after it: %v", err)
	}
	if string(plain) != "value-that-must-outlive-a-restart" {
		// Length only: the assertion is the round-trip, and a mismatch must not print either value.
		t.Fatalf("the secret decrypted to %d bytes, not the value that was stored", len(plain))
	}
}
