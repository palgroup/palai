package stack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyUpgradeCompat(t *testing.T) {
	cases := []struct {
		name          string
		target        string
		current       string
		wantErr       bool
		wantErrSubstr string
	}{
		{"forward within window", "0.15.0", "0.14.0", false, ""},
		{"same version", "0.15.0", "0.15.0", false, ""},
		{"forward at window edge", "0.15.0", "0.13.0", false, ""},
		{"downgrade refused", "0.13.0", "0.15.0", true, "newer than control-plane"},
		{"too-wide skew refused with hop", "0.15.0", "0.11.0", true, "hop to 0.13.0"},
		{"unstamped current is not blocked", "0.15.0", "dev", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := verifyUpgradeCompat(c.target, c.current)
			if c.wantErr && err == nil {
				t.Fatalf("verifyUpgradeCompat(%q,%q) = nil, want an error", c.target, c.current)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("verifyUpgradeCompat(%q,%q) = %v, want nil", c.target, c.current, err)
			}
			if c.wantErr && c.wantErrSubstr != "" && !strings.Contains(err.Error(), c.wantErrSubstr) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantErrSubstr)
			}
		})
	}
}

func TestPreservedRuntimeVarsAreScopedToTheRightContainer(t *testing.T) {
	has := func(list []string, key string) bool {
		for _, k := range list {
			if k == key {
				return true
			}
		}
		return false
	}

	// MF-1: PALAI_RUNNER_CONCURRENCY lives ONLY on the runner service, so it must be read from the runner
	// container — reading it off the control-plane always misses it and the upgrade silently drops a
	// delegation stack's concurrency=2 back to 1 (inline-child deadlock).
	if !has(runnerRuntimeVars, "PALAI_RUNNER_CONCURRENCY") {
		t.Error("PALAI_RUNNER_CONCURRENCY must be a runner-scoped carry-forward var")
	}
	if has(cpRuntimeVars, "PALAI_RUNNER_CONCURRENCY") {
		t.Error("PALAI_RUNNER_CONCURRENCY must NOT be read from the control-plane container (MF-1 regression)")
	}

	// MF-2: the exec-path + retention vars are control-plane-scoped and must all be carried so an upgrade
	// does not silently disable dispatch or the store:false retention reaper.
	for _, key := range []string{"PALAI_DISPATCH_WORKERS", "PALAI_MODEL_PROVIDER", "PALAI_MODEL", "PALAI_RETENTION_STORE_FALSE_TTL", "PALAI_RUNNER_CERT_TTL"} {
		if !has(cpRuntimeVars, key) {
			t.Errorf("%s must be a control-plane-scoped carry-forward var", key)
		}
	}
}

func TestCurrentVersionErrorsWithoutFromOrVersionFile(t *testing.T) {
	// The package dir has no ./VERSION, so with no --from the compat current version cannot be resolved —
	// it must ERROR (SF-6), not fall open to "dev" (which would silently skip the §48.2 compat gate).
	if _, err := currentVersionOrFile(""); err == nil {
		t.Error("currentVersionOrFile(\"\") returned no error; it must demand --from rather than default to dev")
	}
	if v, err := currentVersionOrFile("0.15.0"); err != nil || v != "0.15.0" {
		t.Fatalf("currentVersionOrFile(\"0.15.0\") = (%q,%v), want (\"0.15.0\",nil)", v, err)
	}
}

func TestLoadReleaseManifestRejectsIncomplete(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	full := `{"version":"0.15.0","stamp":"0.15.0+gabc","images":{
		"control_plane":{"ref":"palai/control-plane:n1","digest":"sha256:aa"},
		"runner":{"ref":"palai/runner:n1","digest":"sha256:bb"},
		"engine":{"ref":"palai/reference-engine:n1","digest":"sha256:cc"}}}`
	if _, err := loadReleaseManifest(write("full.json", full)); err != nil {
		t.Fatalf("a complete manifest was rejected: %v", err)
	}

	// A --no-images manifest (empty digests) is refused: the swap has nothing to pin.
	noImages := `{"version":"0.15.0","images":{
		"control_plane":{"ref":"","digest":""},
		"runner":{"ref":"","digest":""},
		"engine":{"ref":"","digest":""}}}`
	if _, err := loadReleaseManifest(write("noimg.json", noImages)); err == nil {
		t.Fatal("a manifest with no image digests was accepted")
	}

	// No version is refused.
	noVersion := `{"images":{"control_plane":{"ref":"x","digest":"sha256:aa"},
		"runner":{"ref":"x","digest":"sha256:bb"},"engine":{"ref":"x","digest":"sha256:cc"}}}`
	if _, err := loadReleaseManifest(write("nover.json", noVersion)); err == nil {
		t.Fatal("a manifest with no version was accepted")
	}
}
