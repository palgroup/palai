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
