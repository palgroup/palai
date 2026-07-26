// The compose-level half of the Slack last mile. E19 T3 built the Socket Mode connect loop and
// tested it; main.startSlackSocket reads PALAI_SLACK_SOCKET_TEAM_ID to turn it on — and the compose
// file, the only deployment an operator actually runs, passed neither that variable nor anything
// that lets a *_ref handle redeem. A registered workspace could therefore never receive an event,
// and nothing said so.
//
// These are static (Docker-free) asserts on the shipped compose file, in the same spirit as
// runner_no_listen_test.go: the invariant is "the wiring exists AND is dormant by default", because
// a knob with a non-empty default would change every stack that has no Slack app at all.
package compose

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeWiring is the slice of the file these asserts read: each service's environment map and
// secret list, plus the top-level secret sources.
type composeWiring struct {
	Services map[string]struct {
		Environment map[string]string `yaml:"environment"`
		Secrets     []string          `yaml:"secrets"`
	} `yaml:"services"`
	Secrets map[string]struct {
		File string `yaml:"file"`
	} `yaml:"secrets"`
}

func loadWiring(t *testing.T) composeWiring {
	t.Helper()
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	var doc composeWiring
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse compose.yaml: %v", err)
	}
	if _, ok := doc.Services["control-plane"]; !ok {
		t.Fatal("compose.yaml has no control-plane service")
	}
	return doc
}

// TestComposePassesTheSlackSocketSelector is Gap 1. main.startSlackSocket is the ONLY start* in the
// control-plane that is conditional on an env var, and compose passed nothing — so the connect loop
// was dormant in every compose deployment no matter what the operator configured.
//
// Both halves are load-bearing: the variable must be PASSED (or Slack can never start) and its
// default must be EMPTY (or every stack without a Slack app changes behaviour).
func TestComposePassesTheSlackSocketSelector(t *testing.T) {
	env := loadWiring(t).Services["control-plane"].Environment
	got, ok := env["PALAI_SLACK_SOCKET_TEAM_ID"]
	if !ok {
		t.Fatal("the control-plane service does not pass PALAI_SLACK_SOCKET_TEAM_ID: the Socket Mode connect loop cannot start in a compose deployment")
	}
	if got != "${PALAI_SLACK_SOCKET_TEAM_ID:-}" {
		t.Fatalf("PALAI_SLACK_SOCKET_TEAM_ID = %q, want ${PALAI_SLACK_SOCKET_TEAM_ID:-} (optional, empty default = dormant)", got)
	}
}

// TestComposeCanRedeemASecretRefHandle is Gap 2. `palai up` registers signing_secret_ref /
// bot_token_ref / app_token_ref — correctly handles, never values — but the base profile mounted no
// way to redeem one: dbSecretStore stays nil without PALAI_SECRET_MASTER_KEY_FILE, and the env-file
// bridge names an org+ref pair a compose `environment:` key cannot express. So every handle
// resolved to nothing and the connection was registered but inert.
//
// The master key is delivered as a file-secret (never an env VALUE) from the SAME
// ${PALAI_HOME}/secrets/master-key path the production overlay already uses, and the env var — a
// PATH — defaults empty, so a stack that never configured one boots exactly as before.
func TestComposeCanRedeemASecretRefHandle(t *testing.T) {
	doc := loadWiring(t)
	cp := doc.Services["control-plane"]

	got, ok := cp.Environment["PALAI_SECRET_MASTER_KEY_FILE"]
	if !ok {
		t.Fatal("the control-plane service does not pass PALAI_SECRET_MASTER_KEY_FILE: dbSecretStore stays nil, so every *_ref handle resolves to nothing")
	}
	if got != "${PALAI_SECRET_MASTER_KEY_FILE:-}" {
		t.Fatalf("PALAI_SECRET_MASTER_KEY_FILE = %q, want ${PALAI_SECRET_MASTER_KEY_FILE:-} (optional, empty default = the secret store stays off)", got)
	}

	var mounted bool
	for _, s := range cp.Secrets {
		if s == "master_key" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("the control-plane mounts %v, without master_key: PALAI_SECRET_MASTER_KEY_FILE would name a path that does not exist inside the container", cp.Secrets)
	}
	if src := doc.Secrets["master_key"].File; src != "${PALAI_HOME}/secrets/master-key" {
		t.Fatalf("the master_key secret source is %q, want ${PALAI_HOME}/secrets/master-key (the path the production overlay and `palai restore verify` already use)", src)
	}
}

// TestComposeCarriesNoSlackCredential is the hygiene half: the wiring above must move HANDLES and
// PATHS only. A Slack credential named in a compose `environment:` value would land in
// `docker inspect` / `compose config` — which is the whole reason the provider key rides a
// file-secret instead.
func TestComposeCarriesNoSlackCredential(t *testing.T) {
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	for _, banned := range []string{"SLACK_SIGNING_SECRET", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "PALAI_SECRET_MASTER_KEY:", "xoxb-", "xapp-"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("compose.yaml names %q: a Slack credential must never ride a compose environment value (it lands in `docker inspect`)", banned)
		}
	}
}
