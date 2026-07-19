//go:build uat

// Package uat's case runner drives the LP-001..015 cases through the packaged local stack
// and captures a redacted evidence bundle. It is behind the `uat` build tag (Docker- and,
// for the live cases, credential-bound) so it never rides make verify. It reuses the shipped
// `palai` CLI as the operator would — no in-process wiring — so the same binary an operator
// runs is what the proof drives.
//
// Deterministic (fake) cases run on a fake stack: no network, no credential. The live-provider
// cases (LP-003/LP-004) run on a provider-one stack against the real provider only when
// PALAI_UAT_PROVIDER=provider-one and OPENAI_API_KEY is in the environment (the operator entry
// loads it from .env.local); otherwise they are skipped. The credential is piped to
// `provider add` over stdin, redeemed only in the broker at call time, and asserted absent
// from every captured surface and from the written manifest.
package uat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type caseSpec struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	ProofClass   string `yaml:"proof_class"`
	Provider     string `yaml:"provider"`
	Input        string `yaml:"input"`
	ExpectStatus string `yaml:"expect_status"`
}

func TestLocalLive(t *testing.T) {
	requireDocker(t)
	release := envOr("PALAI_UAT_RELEASE", "local-live-0.1.0")
	liveEnabled := os.Getenv("PALAI_UAT_PROVIDER") == "provider-one"

	specs := loadCases(t)
	det, live := partition(specs)

	receipts := map[string]caseReceipt{}

	// Deterministic tier: one fake stack, every e2e-deterministic case.
	if len(det) > 0 {
		s := newUATStack(t, "fake", "")
		for _, c := range det {
			receipts[c.ID] = s.runCase(t, c)
		}
		s.reset()
	}

	// Live tier: a provider-one stack against the real provider. Skipped without the operator
	// flag + credential so make/CI never needs a key; the live proof is its own protected run.
	if len(live) > 0 {
		if !liveEnabled {
			t.Skipf("live-provider cases need PALAI_UAT_PROVIDER=provider-one and OPENAI_API_KEY (run make uat-local-live PROVIDER=provider-one)")
		}
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			t.Fatal("OPENAI_API_KEY is unset; the operator entry loads it from .env.local")
		}
		s := newUATStack(t, "provider-one", key)
		for _, c := range live {
			receipts[c.ID] = s.runCase(t, c)
		}
		s.reset()
	}

	// Assemble the redacted manifest in case order and write the bundle.
	manifest := buildManifest(t, release, specs, receipts)
	dir := filepath.Join(repoRoot(t), "evidence", "releases", release)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make release dir: %v", err)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// The bundle must verify clean, and the live credential must never appear in it.
	var needles []string
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		needles = append(needles, k)
	}
	summary, err := VerifyRelease(dir, needles)
	if err != nil {
		t.Fatalf("verify bundle: %v", err)
	}
	for _, c := range specs {
		if _, ran := receipts[c.ID]; ran {
			fmt.Printf("%s PASS\n", c.ID)
		}
	}
	fmt.Printf("evidence: %s\n", summary.String())
	if !summary.OK() {
		t.Fatalf("evidence bundle did not verify: %v", summary.Findings)
	}
}

// caseReceipt is the captured evidence for one case.
type caseReceipt struct {
	RunID             string
	ImageDigest       string
	ProviderRequestID string
	MTLSEnroll        string
	TerminalType      string
	TerminalCount     int
	Usage             map[string]int
	DBAssertions      []string
	Checksum          string
}

// partition splits cases into the deterministic and live-provider tiers.
func partition(specs []caseSpec) (det, live []caseSpec) {
	for _, c := range specs {
		if c.Provider == "provider-one" {
			live = append(live, c)
		} else {
			det = append(det, c)
		}
	}
	return det, live
}

// loadCases reads every tests/uat/cases/*/case.yaml in sorted id order.
func loadCases(t *testing.T) []caseSpec {
	t.Helper()
	root := filepath.Join(repoRoot(t), "tests", "uat", "cases")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	var specs []caseSpec
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "case.yaml"))
		if err != nil {
			t.Fatalf("read %s/case.yaml: %v", e.Name(), err)
		}
		var c caseSpec
		if err := yaml.Unmarshal(raw, &c); err != nil {
			t.Fatalf("decode %s/case.yaml: %v", e.Name(), err)
		}
		specs = append(specs, c)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].ID < specs[j].ID })
	return specs
}

// buildManifest rolls the receipts into the evidence manifest, in case order.
func buildManifest(t *testing.T, release string, specs []caseSpec, receipts map[string]caseReceipt) map[string]any {
	t.Helper()
	var cases []map[string]any
	for _, c := range specs {
		r, ran := receipts[c.ID]
		if !ran {
			continue
		}
		cases = append(cases, map[string]any{
			"id":                  c.ID,
			"status":              "PASS",
			"proof_class":         c.ProofClass,
			"run_id":              r.RunID,
			"image_digest":        r.ImageDigest,
			"provider_request_id": r.ProviderRequestID,
			"mtls_enroll":         r.MTLSEnroll,
			"terminal":            map[string]any{"type": r.TerminalType, "count": r.TerminalCount},
			"usage":               r.Usage,
			"db_assertions":       r.DBAssertions,
			"checksum":            r.Checksum,
		})
	}
	return map[string]any{
		"release":     release,
		"git_sha":     gitSHA(t),
		"api_version": "v1",
		"migration":   latestMigration(t),
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"cases":       cases,
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func gitSHA(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoRoot(t), "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// latestMigration returns the highest migration version applied, e.g. 000002_retention.
func latestMigration(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "storage", "migrations"))
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var ups []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, strings.TrimSuffix(e.Name(), ".up.sql"))
		}
	}
	sort.Strings(ups)
	if len(ups) == 0 {
		t.Fatal("no migrations found")
	}
	return ups[len(ups)-1]
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not available: %v", err)
	}
}

// hashBundle is the checksum over a case's captured canonical surfaces.
func hashBundle(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// redactBytes is a defensive scrub: the credential is never written to a captured surface, but
// evidence is passed through this before it is recorded so an accidental echo cannot leak.
func redactBytes(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}
