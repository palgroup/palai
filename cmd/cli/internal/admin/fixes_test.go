package admin

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePalai writes a minimal initialised .palai (config.json + api-key) and points PALAI_HOME at it.
func writePalai(t *testing.T, baseURL, bootstrapKey string) {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"base_url":"`+baseURL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if bootstrapKey != "" {
		if err := os.WriteFile(filepath.Join(home, "api-key"), []byte(bootstrapKey+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PALAI_HOME", home)
	t.Setenv("PALAI_BASE_URL", "")
	t.Setenv("PALAI_API_KEY", "")
}

// TestExplicitEmptyKeyFileErrors is the MUST-FIX 1 guard: an explicitly named --api-key-file that is empty
// after trim must ERROR — never silently fall back to the .palai bootstrap admin key (privilege
// substitution). A failed `vault read > /tmp/k` must not run the command as full admin.
func TestExplicitEmptyKeyFileErrors(t *testing.T) {
	writePalai(t, "http://from-palai:8080", "palai-bootstrap-admin")

	empty := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, apiKey, err := resolve("", empty)
	if err == nil {
		t.Fatalf("empty --api-key-file must error; instead resolved apiKey=%q (would run as bootstrap admin)", apiKey)
	}
	if apiKey == "palai-bootstrap-admin" {
		t.Fatal("privilege substitution: an empty named key file fell back to the bootstrap admin key")
	}
}

// TestBaseURLFromPalaiWhenKeyFromEnv is SHOULD 4: the key resolves from $PALAI_API_KEY and the base URL
// from .palai — a .palai api-key read problem must NOT be fatal when nothing still needs it.
func TestBaseURLFromPalaiWhenKeyFromEnv(t *testing.T) {
	home := t.TempDir()
	// config.json present (base URL resolvable) but NO api-key file (AdminDefaults returns a read error).
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"base_url":"http://from-palai:9090"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALAI_HOME", home)
	t.Setenv("PALAI_BASE_URL", "")
	t.Setenv("PALAI_API_KEY", "env-key")

	baseURL, apiKey, err := resolve("", "")
	if err != nil {
		t.Fatalf("resolve should succeed (key from env, base URL from .palai): %v", err)
	}
	if baseURL != "http://from-palai:9090" || apiKey != "env-key" {
		t.Fatalf("baseURL=%q apiKey=%q, want the .palai base URL + env key", baseURL, apiKey)
	}
}

// TestSecretValueTooLarge is SHOULD 2: a value over 64KiB must error, not silently truncate.
func TestSecretValueTooLarge(t *testing.T) {
	big := strings.Repeat("a", 64*1024+1)
	if _, err := readSecret(strings.NewReader(big)); err == nil {
		t.Fatal("a >64KiB secret must error, not truncate")
	}
	// Exactly 64KiB (plus a shell newline) is fine.
	ok := strings.Repeat("a", 64*1024)
	if v, err := readSecret(strings.NewReader(ok + "\n")); err != nil || len(v) != 64*1024 {
		t.Fatalf("a 64KiB secret must be accepted: len=%d err=%v", len(v), err)
	}
}

// TestSecretValueNonUTF8 is SHOULD 3: an invalid-UTF-8 value must error rather than be corrupted to U+FFFD.
func TestSecretValueNonUTF8(t *testing.T) {
	if _, err := readSecret(bytes.NewReader([]byte{0xff, 0xfe, 0x00})); err == nil {
		t.Fatal("a non-UTF-8 secret must error (base64-encode binary values)")
	}
}

// TestSecretRejectsTTY is SHOULD 6: reading a secret from a character device (a terminal) is refused, so a
// typed secret never echoes to the screen/scrollback. /dev/null is a character device, so it stands in.
func TestSecretRejectsTTY(t *testing.T) {
	dev, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer dev.Close()
	_, err = readSecret(dev)
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("reading a secret from a character device (TTY) must be refused as a terminal, got %v", err)
	}
}

// TestExtraPositionalRejected is SHOULD 5: an unexpected extra positional is an error — it catches a second
// id silently ignored (`revoke key_1 key_2`) and a secret fat-fingered onto argv
// (`secret create --name x SECRET`), the latter without ever sending it.
func TestExtraPositionalRejected(t *testing.T) {
	var cap capture
	srv := stubServer(t, http.StatusOK, `{}`, &cap)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "k")

	// A second id must not silently revoke only the first.
	cap = capture{}
	if err := Run("apikey", []string{"revoke", "key_1", "key_2"}, new(bytes.Buffer), strings.NewReader("")); err == nil {
		t.Fatal("`revoke key_1 key_2` must error, not silently revoke only key_1")
	}
	if cap.method != "" {
		t.Fatalf("no request should have gone out for an arity error, saw %s %s", cap.method, cap.path)
	}

	// A secret positioned on argv (arity-0 create) must error WITHOUT sending anything.
	cap = capture{}
	if err := Run("secret", []string{"create", "--name", "x", "SECRET-ON-ARGV"}, new(bytes.Buffer), strings.NewReader("v")); err == nil {
		t.Fatal("`secret create --name x SECRET` must error (extra positional), protecting an argv-borne secret")
	}
	if cap.method != "" {
		t.Fatalf("no request should go out when a secret is fat-fingered onto argv, saw %s %s", cap.method, cap.path)
	}
}
