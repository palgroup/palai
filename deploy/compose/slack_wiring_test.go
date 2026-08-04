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

// TestComposePassesTheSlackSocketSelector WAS HERE AND IS GONE (cutover, 2026-08-05). It asserted that
// compose passed PALAI_SLACK_SOCKET_TEAM_ID with an EMPTY default — both halves load-bearing, because the
// control plane's Socket Mode loop could not start without the variable and every Slack-less stack would
// have changed behaviour with a non-empty one.
//
// The loop is deleted. apps/slack-bot holds the connection now, one process per registered bot row, reading
// its own row through /v1 — so compose configures nothing Slack-shaped and there is no selector to pass. The
// variable is gone from compose.yaml, from the deployment catalogue and from the CLI's config validator in
// the same change.
//
// THE OTHER THREE TESTS IN THIS FILE STAY and none of them is about Slack's transport: they assert that a
// secret-ref handle can be redeemed at all, that the object store reaches the control plane, and that no
// Slack CREDENTIAL is ever a compose environment value. The last one matters more after this cutover rather
// than less — the bot's credentials live in the secret store and a compose file is exactly where somebody
// would be tempted to paste one.

// TestComposeCanRedeemASecretRefHandle is Gap 2. `palai up` registers signing_secret_ref /
// bot_token_ref / app_token_ref — correctly handles, never values — but the base profile mounted no
// way to redeem one: dbSecretStore stays nil without PALAI_SECRET_MASTER_KEY_FILE, and the env-file
// bridge names an org+ref pair a compose `environment:` key cannot express. So every handle
// resolved to nothing and the connection was registered but inert.
//
// The master key is delivered as a file-secret (never an env VALUE) from the SAME
// ${PALAI_HOME}/secrets/master-key path the production overlay already uses.
//
// E21 T2 (§3.6 D5) CHANGED THE EXPECTATION HERE, and the change is the fix. This assertion used to
// demand `${PALAI_SECRET_MASTER_KEY_FILE:-}` — interpolated, empty by default — reasoning that a stack
// which configured no key should boot exactly as before. Measured, that default was not "the secret
// store stays off", it was "the secret store is off unless the operator happened to have Slack
// credentials in .env.local", because `palai up`'s applySlackEnv was the one and only exporter of the
// variable. A plain `docker compose up -d` produced an inert stack whose every handle resolved
// nowhere. The path is written LITERALLY now, as production.yml has always written it, and the CLI's
// ensureSecretSlots mints the key it names on every bring-up.
func TestComposeCanRedeemASecretRefHandle(t *testing.T) {
	doc := loadWiring(t)
	cp := doc.Services["control-plane"]

	got, ok := cp.Environment["PALAI_SECRET_MASTER_KEY_FILE"]
	if !ok {
		t.Fatal("the control-plane service does not pass PALAI_SECRET_MASTER_KEY_FILE: dbSecretStore stays nil, so every *_ref handle resolves to nothing")
	}
	if got != "/run/secrets/master_key" {
		t.Fatalf("PALAI_SECRET_MASTER_KEY_FILE = %q, want the literal /run/secrets/master_key. An interpolated value is read from "+
			"whatever shell invoked compose, and nothing but `palai up`-with-Slack-credentials ever sets it — so the secret store "+
			"silently stays nil and every *_ref handle resolves nowhere (§3.6 D5)", got)
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

// TestComposeWiresTheObjectStoreIntoTheControlPlane is Gap 3, and it is the same shape as the two above:
// something the operator runs was BUILT and then never connected.
//
// WHAT IT COST. The owner shared a screenshot and asked what was in it; the run's input said, honestly, "a
// file shared with this message could not be attached". The image leg (E20, slack_vision.go) is mounted in
// main.go — `if artStore != nil { WithFileFetch(...) }` — but artStore comes from artifactStoreFromEnv, which
// returns nil unless PALAI_S3_ENDPOINT is set, and THIS FILE has always started an object-store service,
// health-checked it, published its port and made the control-plane depend_on it while telling the
// control-plane nothing about where it is. So every compose deployment ran an object store nothing wrote to.
//
// The Slack image leg is only the loudest symptom. Everything gated on artStore is off the same way: the
// artifact retrieval read-path, the checkpoint and snapshot sinks, retention's byte deletion and the orphan
// collector. artifacts.Config's own comment already names the value this test asserts
// ("e.g. http://object-store:8333 (compose)") — the code was written for a wire that was never run.
//
// UNLIKE the two gaps above this one has NO empty default, and that is deliberate: the object store is not
// optional configuration an operator opts into, it is a service this same file starts unconditionally. An
// interpolated `${...:-}` here would reproduce the bug the moment somebody ran compose without an env file.
func TestComposeWiresTheObjectStoreIntoTheControlPlane(t *testing.T) {
	doc := loadWiring(t)
	if _, ok := doc.Services["object-store"]; !ok {
		t.Fatal("compose.yaml has no object-store service; this test's premise is gone, not satisfied")
	}
	env := doc.Services["control-plane"].Environment

	got, ok := env["PALAI_S3_ENDPOINT"]
	if !ok {
		t.Fatal("the control-plane service does not pass PALAI_S3_ENDPOINT: artifactStoreFromEnv returns nil, so " +
			"the Slack image leg is never mounted (a shared screenshot is silently admitted as text), artifact " +
			"retrieval is unmounted and retention deletes no bytes — while this same file runs an object store")
	}
	// The service NAME on the compose network, not a host port: the control-plane reaches its peer inside the
	// network, and 127.0.0.1:${PALAI_S3_PORT} is where the HOST reaches it. Getting this wrong fails at boot
	// (EnsureBucket is fatal), which is loud — but it is still worth pinning, because "it booted" is the only
	// evidence anyone has that the store is wired.
	if want := "http://object-store:8333"; got != want {
		t.Fatalf("PALAI_S3_ENDPOINT = %q, want %q — the control-plane must address the object store by its "+
			"service name on the compose network, not by a published host port", got, want)
	}
	if bucket, ok := env["PALAI_S3_BUCKET"]; !ok || bucket == "" {
		t.Fatalf("PALAI_S3_BUCKET = %q (present=%t); artifacts.NewStore requires a non-empty bucket and "+
			"main.go's envDefault fallback is not something a deployment should rely on being there", bucket, ok)
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
