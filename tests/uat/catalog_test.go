package uat

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// catalogCase is the case.yaml catalog record — the same fields the uat-tagged caseSpec decodes, read
// here in the untagged tier so the catalog is gated by make verify (no Docker, no credential).
type catalogCase struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	ProofClass   string `yaml:"proof_class"`
	Provider     string `yaml:"provider"`
	Input        string `yaml:"input"`
	ExpectStatus string `yaml:"expect_status"`
}

// validProofClasses is the master-plan §10.2 proof-class vocabulary a case may declare. A typo or an
// invented class fails the catalog rather than silently under-proving.
var validProofClasses = map[string]bool{
	"unit": true, "component-real": true, "e2e-deterministic": true,
	"live-provider": true, "external-receipt": true, "fault-live": true,
}

var honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// TestCodingCaseCatalogWellFormed is the coding UAT registry gate: every coding case.yaml (the
// REP/SUB/REG/DEL/APV cases this slice owns) declares a valid proof class, an honest kebab-case name
// asserting the behaviour, a provider, and the case.yaml discipline's remaining fields — and its id
// matches its directory. It runs in make verify so a malformed or overclaiming catalog entry fails fast,
// and it deliberately EXCLUDES the LP-* cases (owned by the local-live release, exercised by TestLocalLive).
func TestCodingCaseCatalogWellFormed(t *testing.T) {
	root := filepath.Join(repoRootUntagged(t), "tests", "uat", "cases")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	coding := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "LP-") {
			continue
		}
		coding++
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "case.yaml"))
		if err != nil {
			t.Fatalf("%s: read case.yaml: %v", e.Name(), err)
		}
		var c catalogCase
		if err := yaml.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: decode case.yaml: %v", e.Name(), err)
		}
		if c.ID != e.Name() {
			t.Errorf("%s: id = %q, want the directory name", e.Name(), c.ID)
		}
		if !validProofClasses[c.ProofClass] {
			t.Errorf("%s: proof_class = %q, not a master-plan §10.2 class", c.ID, c.ProofClass)
		}
		if !honestNamePattern.MatchString(c.Name) {
			t.Errorf("%s: name = %q, want a kebab-case behaviour assertion", c.ID, c.Name)
		}
		if c.Provider == "" || c.Input == "" || c.ExpectStatus == "" {
			t.Errorf("%s: provider/input/expect_status must all be set (case.yaml discipline)", c.ID)
		}
		// An external-receipt or live-provider case cannot silently be a fake local run: it declares the
		// tier its receipt genuinely comes from. A fake provider on a live-provider case would overclaim.
		if c.ProofClass == "live-provider" && c.Provider == "fake" {
			t.Errorf("%s: a live-provider case must not declare the fake provider", c.ID)
		}
	}
	if coding == 0 {
		t.Fatal("no coding cases found under tests/uat/cases (expected REP/SUB/REG/DEL/APV)")
	}
}

// repoRootUntagged walks up to the module root. It duplicates the uat-tagged repoRoot so the catalog gate
// compiles in the untagged tier (repoRoot lives behind the uat build tag).
func repoRootUntagged(t *testing.T) string {
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
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
