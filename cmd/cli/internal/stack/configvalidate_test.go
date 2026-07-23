package stack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A byte-for-byte copy of the production overlay's posture that config-validate reads: the four
// internal services reset their host ports and only the edge publishes, and the Caddyfile
// reverse_proxy is path-matched to /v1/* (so /metrics + /healthz are not edge-reachable). The
// tests mutate this to prove each machine-check catches a re-exposed surface.
const goodOverlay = `services:
  postgres:
    restart: always
    ports: !reset []
  object-store:
    restart: always
    ports: !reset []
  control-plane:
    restart: always
    ports: !reset []
  runner:
    restart: always
  edge:
    image: caddy@sha256:deadbeef
    ports:
      - "${PALAI_EDGE_PORT:?PALAI_EDGE_PORT must be set}:443"
configs:
  edge_caddyfile:
    content: |
      {
      	auto_https off
      }
      :443 {
      	tls /etc/palai/edge/edge.crt /etc/palai/edge/edge.key
      	reverse_proxy /v1/* control-plane:8080
      }
`

// The dev-default literals are read from production-entrypoint.sh, never re-declared here — this
// mirrors the exact assignment shape the guard hardcodes.
const guardScript = `#!/bin/sh
DEV_MASTER_KEY_PLACEHOLDER="REPLACE_WITH_OPENSSL_RAND_HEX_32"
DEV_MASTER_KEY_ZERO="0000000000000000000000000000000000000000000000000000000000000000"
DEV_BOOTSTRAP_KEY_PLACEHOLDER="REPLACE_WITH_A_REAL_BOOTSTRAP_KEY"
exec "$@"
`

const goodEnv = `PALAI_HOME=%s
PALAI_EDGE_PORT=443
PALAI_ENGINE_IMAGE=palai/reference-engine:local
PALAI_COMPOSE_PROJECT=palai-prod
PALAI_DISPATCH_WORKERS=1
PALAI_MODEL_PROVIDER=fake
PALAI_MODEL=
`

// prodLayout writes a valid production layout (env-file, overlay, entrypoint, and a PALAI_HOME
// with a real master key, real bootstrap key, and a cert pair) into a temp dir and returns the
// env-file and overlay paths config-validate reads.
func prodLayout(t *testing.T) (envFile, overlay, home string) {
	t.Helper()
	dir := t.TempDir()
	home = filepath.Join(dir, "home")
	for _, sub := range []string{"secrets", "ca"} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, v string) {
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "secrets", "master-key"), "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90")
	write(filepath.Join(home, "api-key"), "palai-realbootstrapkeyvalue")
	write(filepath.Join(home, "ca", "server.crt"), "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n")
	write(filepath.Join(home, "ca", "server.key"), "-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n")

	envFile = filepath.Join(dir, "production.env")
	write(envFile, strings.Replace(goodEnv, "%s", home, 1))
	overlay = filepath.Join(dir, "production.yml")
	write(overlay, goodOverlay)
	write(filepath.Join(dir, "production-entrypoint.sh"), guardScript)
	return envFile, overlay, home
}

func TestConfigValidatePassesOnGoodConfig(t *testing.T) {
	envFile, overlay, _ := prodLayout(t)
	if err := ConfigValidate(envFile, overlay, true); err != nil {
		t.Fatalf("valid production config should pass, got: %v", err)
	}
}

func TestConfigValidateRejectsDevMasterKey(t *testing.T) {
	envFile, overlay, home := prodLayout(t)
	// The literal placeholder the shipped example carries — the guard and config-validate must
	// agree it is not a real key.
	if err := os.WriteFile(filepath.Join(home, "secrets", "master-key"),
		[]byte("REPLACE_WITH_OPENSSL_RAND_HEX_32"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ConfigValidate(envFile, overlay, true); err == nil {
		t.Fatal("a dev-default master key must fail config validate")
	}
}

func TestConfigValidateRejectsReExposedPort(t *testing.T) {
	envFile, overlay, _ := prodLayout(t)
	// Re-publish the control-plane API to the host — the edge is no longer the only surface.
	reexposed := strings.Replace(goodOverlay,
		"  control-plane:\n    restart: always\n    ports: !reset []",
		"  control-plane:\n    restart: always\n    ports:\n      - \"8080:8080\"", 1)
	if err := os.WriteFile(overlay, []byte(reexposed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ConfigValidate(envFile, overlay, true); err == nil {
		t.Fatal("a re-exposed internal host port must fail config validate")
	}
}

func TestConfigValidateRejectsMissingEnv(t *testing.T) {
	envFile, overlay, _ := prodLayout(t)
	if err := os.WriteFile(envFile, []byte("PALAI_HOME=/tmp/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ConfigValidate(envFile, overlay, true); err == nil {
		t.Fatal("an env file missing required keys must fail config validate")
	}
}

func TestParseDevDefaultsMatchesGuardLiterals(t *testing.T) {
	// The real shipped guard is the source of truth; config-validate must extract exactly its set.
	script, err := os.ReadFile("../../../../deploy/compose/production-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	dd, err := parseDevDefaults(string(script))
	if err != nil {
		t.Fatal(err)
	}
	wantMaster := map[string]bool{
		"REPLACE_WITH_OPENSSL_RAND_HEX_32":                                 false,
		"0000000000000000000000000000000000000000000000000000000000000000": false,
	}
	for _, m := range dd.masterKeys {
		if _, ok := wantMaster[m]; ok {
			wantMaster[m] = true
		}
	}
	for k, seen := range wantMaster {
		if !seen {
			t.Fatalf("parseDevDefaults did not extract master default %q from the guard", k)
		}
	}
	if !containsStr(dd.bootstrapKeys, "REPLACE_WITH_A_REAL_BOOTSTRAP_KEY") {
		t.Fatalf("bootstrap defaults %v miss the guard's placeholder", dd.bootstrapKeys)
	}
}

func TestParseDevDefaultsKeepsAllBootstrapLiterals(t *testing.T) {
	// A second DEV_BOOTSTRAP_* added to the guard must NOT overwrite the first (last-wins would
	// green-light every bootstrap key but the last). Both must be collected, like the master set.
	script := `DEV_BOOTSTRAP_KEY_PLACEHOLDER="FIRST_BOOT"
DEV_BOOTSTRAP_KEY_LEGACY="SECOND_BOOT"
DEV_MASTER_KEY_PLACEHOLDER="M"`
	dd, err := parseDevDefaults(script)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(dd.bootstrapKeys, "FIRST_BOOT") || !containsStr(dd.bootstrapKeys, "SECOND_BOOT") {
		t.Fatalf("both bootstrap literals must be kept, got %v", dd.bootstrapKeys)
	}
}

func TestConfigValidateRejectsNonHexMasterKey(t *testing.T) {
	envFile, overlay, home := prodLayout(t)
	// Not a dev-default, but not a 32-byte hex key either — it passes validate today then fails at
	// boot (identity.ParseMasterKey). Close the will-it-boot gap.
	if err := os.WriteFile(filepath.Join(home, "secrets", "master-key"),
		[]byte("this-is-not-hex-and-not-64-chars"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ConfigValidate(envFile, overlay, true); err == nil {
		t.Fatal("a non-hex master key must fail config validate")
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestEdgeSurfaceRejectsUnresetPorts(t *testing.T) {
	// Compose merges port LISTS by appending, so a plain `ports: []` (no !reset tag) and an omitted
	// ports key BOTH leave base compose.yaml's published control-plane port in the merged config —
	// /metrics + /healthz stay host-reachable. Only `!reset []`/`!override []` removes them. The
	// machine-truth check must FAIL both, not green-light a posture that is false.
	plainEmpty := strings.Replace(goodOverlay,
		"  postgres:\n    restart: always\n    ports: !reset []",
		"  postgres:\n    restart: always\n    ports: []", 1)
	if c := edgeSurfaceCheck(plainEmpty); c.Status == "ok" {
		t.Fatalf("plain `ports: []` (append-merge keeps base ports) must FAIL, got ok: %s", c.Detail)
	}
	noKey := strings.Replace(goodOverlay,
		"  control-plane:\n    restart: always\n    ports: !reset []",
		"  control-plane:\n    restart: always", 1)
	if c := edgeSurfaceCheck(noKey); c.Status == "ok" {
		t.Fatalf("a base-published service with no ports key (base ports stay) must FAIL, got ok: %s", c.Detail)
	}
	// An explicit !override [] also removes base ports → still OK.
	override := strings.Replace(goodOverlay, "ports: !reset []", "ports: !override []", 3)
	if c := edgeSurfaceCheck(override); c.Status != "ok" {
		t.Fatalf("!override [] should reset ports and pass, got %q: %s", c.Status, c.Detail)
	}
}

func TestEdgeSurfaceRejectsCatchAllProxy(t *testing.T) {
	// A reverse_proxy with no path matcher catches everything, including /metrics — the exact
	// leak the /v1/* match prevents.
	bad := strings.Replace(goodOverlay, "reverse_proxy /v1/* control-plane:8080", "reverse_proxy control-plane:8080", 1)
	if c := edgeSurfaceCheck(bad); c.Status == "ok" {
		t.Fatalf("a catch-all reverse_proxy must fail the edge-surface check, got ok: %s", c.Detail)
	}
	if c := edgeSurfaceCheck(goodOverlay); c.Status != "ok" {
		t.Fatalf("the /v1/*-matched edge should pass, got %q: %s", c.Status, c.Detail)
	}
}
