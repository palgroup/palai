package stack

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// moduleRoot walks up to the module root. It duplicates up_e2e_test.go's repoRoot under a different
// name on purpose: that one lives behind the e2e build tag, and these proofs must run untagged.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// TestMasterKeyPointerIsNotInterpolatedFromTheInvokingShell is the E21 T2 / §3.6 D5 RED, and the
// symptom it pins is the one the owner met as "secrets do not survive a restart".
//
// They always did. Secrets live in Postgres, envelope-encrypted, and identity/secrets.go re-reads the
// row on every resolve — there is no in-memory cache to lose. What was lost is the POINTER: this line
// interpolated PALAI_SECRET_MASTER_KEY_FILE from the shell that invoked compose, and the only thing
// that ever exported it was the bring-up's Slack env step, which ran ONLY when .env.local held Slack
// credentials. So a plain `docker compose up -d`, or any `palai up` on a stack with no Slack app,
// handed the container an EMPTY value, left dbSecretStore nil, unmounted the secret-ref routes, and
// every handle resolved nowhere. deploy/compose/production.yml writes the path literally and has
// never had this bug; the local file must match it.
func TestMasterKeyPointerIsNotInterpolatedFromTheInvokingShell(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	line := ""
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "PALAI_SECRET_MASTER_KEY_FILE:") {
			line = strings.TrimSpace(l)
			break
		}
	}
	if line == "" {
		t.Fatal("compose.yaml no longer sets PALAI_SECRET_MASTER_KEY_FILE at all: the DB-backed secret store " +
			"would stay nil on every local stack and every *_ref handle would resolve nowhere")
	}
	if strings.Contains(line, "${") {
		t.Fatalf("PALAI_SECRET_MASTER_KEY_FILE is interpolated from the invoking shell (%q). "+
			"A `docker compose up -d` — or a `palai up` on a stack with no Slack credentials — then passes an EMPTY "+
			"value, dbSecretStore stays nil, and every secret handle resolves nowhere. Write the path literally, "+
			"as production.yml already does", line)
	}
	if !strings.Contains(line, containerMasterKeyPath) {
		t.Fatalf("PALAI_SECRET_MASTER_KEY_FILE = %q, want the compose file-secret mount %s", line, containerMasterKeyPath)
	}
}

// TestEveryBringUpLeavesABootableMasterKey is the other half of the same fix, and without it the line
// above is a way to BRICK the stack rather than repair it.
//
// The control-plane treats a SET-but-unparseable PALAI_SECRET_MASTER_KEY_FILE as log.Fatalf (main.go:
// "a broken key-file permission on redeploy must not boot healthy with the secret store silently
// disabled"). ensureSecretSlots used to create the master-key slot EMPTY — which was safe only while
// nothing named it. Now that compose names it unconditionally, every bring-up must leave a key the
// control-plane can parse, or `palai local up` boots into a crash loop.
//
// ensureSecretSlots is the right home because it is the ONE funnel every compose bring-up routes
// through: composeUp() calls it, `palai local up` reaches it through Up(), and Bootstrap (`palai up`)
// drives composeUp() directly.
func TestEveryBringUpLeavesABootableMasterKey(t *testing.T) {
	p := tempPaths(t)
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("ensure the secret slots: %v", err)
	}
	key, err := readTrimmed(p.masterKey)
	if err != nil {
		t.Fatalf("read the master key slot: %v", err)
	}
	if len(key) != 64 || !isHex(key) {
		// The value is a KEY: report its shape, never its bytes.
		t.Fatalf("the master-key slot holds %d bytes of non-key material after a bring-up; compose now names this "+
			"file unconditionally, so the control-plane would log.Fatalf on it and the stack would never come up", len(key))
	}
	// A second bring-up must not re-mint: the key seals every secret already in the store, and dbSecret
	// fails CLOSED on a decrypt failure — re-minting would not degrade the stack, it would break it.
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	again, _ := readTrimmed(p.masterKey)
	if again != key {
		t.Fatal("a second bring-up replaced the master key: every secret already sealed under the old one is now dead")
	}
}

// TestMasterKeyIsMintedOnceAndNeverReplaced: the master key seals every secret in the store, so a
// second `palai up` that re-minted it would make every previously stored credential undecryptable — and
// the fail-closed resolver would then refuse rather than fall back, taking every *_ref handle on the
// stack down with it.
func TestMasterKeyIsMintedOnceAndNeverReplaced(t *testing.T) {
	p := tempPaths(t)
	if err := ensureMasterKey(p); err != nil {
		t.Fatalf("mint the master key: %v", err)
	}
	first, err := os.ReadFile(p.masterKey)
	if err != nil {
		t.Fatalf("read the minted key: %v", err)
	}
	if _, err := hex.DecodeString(strings.TrimSpace(string(first))); err != nil || len(strings.TrimSpace(string(first))) != 64 {
		t.Fatalf("the minted key is not 64 hex chars (identity.ParseMasterKey's AES-256 contract): %d chars", len(first))
	}
	info, err := os.Stat(p.masterKey)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the master key file is mode %v, want 0600", info.Mode().Perm())
	}
	if err := ensureMasterKey(p); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	second, _ := os.ReadFile(p.masterKey)
	if string(second) != string(first) {
		t.Fatal("the master key was replaced on a second bring-up: every secret already sealed under it is now undecryptable")
	}
}

// TestMasterKeyRefusesToGuessAtAnUnusableOne: a file holding something that is not a 32-byte hex key
// makes the control-plane log.Fatalf at boot (identity.ParseMasterKey is a startup error by design).
// Overwriting it would destroy whatever it was; booting on it would take the stack down. Refuse and
// say so instead.
func TestMasterKeyRefusesToGuessAtAnUnusableOne(t *testing.T) {
	p := tempPaths(t)
	if err := os.WriteFile(p.masterKey, []byte("REPLACE_WITH_OPENSSL_RAND_HEX_32"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ensureMasterKey(p)
	if err == nil {
		t.Fatal("an unusable master key was accepted; the control-plane would refuse to boot on it")
	}
	if !strings.Contains(err.Error(), p.masterKey) {
		t.Fatalf("the refusal must name the file so it can be fixed, got: %v", err)
	}
	after, _ := os.ReadFile(p.masterKey)
	if string(after) != "REPLACE_WITH_OPENSSL_RAND_HEX_32" {
		t.Fatal("the existing master key was overwritten")
	}
}

// TestEverySecretSlotComposeMountsExists: compose bind-mounts each file-secret and a MISSING source
// fails `compose up` outright — which is why `palai init` has always created the provider-one slot
// empty. The master-key slot joins it, and a stack initialised by an earlier build (whose .palai has
// no such file, and whose `init` short-circuits on an existing config.json) must be repaired by the
// bring-up rather than left unable to start.
func TestEverySecretSlotComposeMountsExists(t *testing.T) {
	p := tempPaths(t)
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("ensure the secret slots: %v", err)
	}
	for _, path := range []string{p.secretPath("provider-one"), p.masterKey} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("compose mounts %s as a file-secret and it does not exist: %v", filepath.Base(path), err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is mode %v, want 0600", filepath.Base(path), info.Mode().Perm())
		}
	}
	// A filled slot is never clobbered by a later bring-up.
	if err := os.WriteFile(p.secretPath("provider-one"), []byte("sk-already-here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if got, _ := os.ReadFile(p.secretPath("provider-one")); string(got) != "sk-already-here" {
		t.Fatalf("a stored credential was cleared by the slot ensure: %q", got)
	}
}

// --- helpers ---------------------------------------------------------------------------------------

// tempPaths builds a paths rooted at a temp dir, so the slot/key helpers can be driven without
// touching the developer's real .palai.
func tempPaths(t *testing.T) paths {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	return paths{
		home:       home,
		secretsDir: filepath.Join(home, "secrets"),
		masterKey:  filepath.Join(home, "secrets", "master-key"),
	}
}

func envGetter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// fakeProvisioningAPI stands in for a running control-plane's repository-binding surface. It records
// every path so a test can assert the bring-up opened NO new route, and it keeps created state so a
// second resolve can reuse.
func fakeProvisioningAPI(t *testing.T) (*apiClient, *[]string) {
	t.Helper()
	var calls []string
	// The repository-binding surface (E22 T3), modelled the way the real one behaves: a re-POST registers a
	// DISTINCT binding (bindings are durable configuration, not idempotent operations — api/repository_bindings.go
	// says so), so a bring-up that does not look before it creates will visibly pile them up here.
	var bindings []map[string]any
	bindingSeq := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		write := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/repository-bindings":
			write(http.StatusOK, map[string]any{"object": "list", "data": bindings})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/repository-bindings":
			var body struct {
				Provider           string `json:"provider"`
				RepositoryIdentity string `json:"repository_identity"`
				CloneURL           string `json:"clone_url"`
				DefaultBranch      string `json:"default_branch"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			// The real handler's own refusal (provider, repository_identity and clone_url are required), so a
			// bring-up that derives an empty identity fails HERE rather than passing against a lenient fake.
			if body.Provider == "" || body.RepositoryIdentity == "" || body.CloneURL == "" {
				write(http.StatusBadRequest, map[string]any{"detail": "provider, repository_identity, and clone_url are required"})
				return
			}
			bindingSeq++
			binding := map[string]any{
				"id": fmt.Sprintf("repo_fake_%d", bindingSeq), "object": "repository_binding",
				"provider": body.Provider, "repository_identity": body.RepositoryIdentity,
				"clone_url": body.CloneURL, "default_branch": body.DefaultBranch,
			}
			bindings = append(bindings, binding)
			write(http.StatusCreated, binding)
		default:
			write(http.StatusNotFound, map[string]any{"detail": "no such route " + r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)
	return &apiClient{baseURL: srv.URL, key: "test", http: &http.Client{Timeout: 5 * time.Second}}, &calls
}
