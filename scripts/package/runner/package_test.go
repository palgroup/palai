// E14 T5 — the package gate: build the signed runner host package, prove it verifies, prove it
// is deterministic (byte-identical rebuild), and prove verify FAILS on a single flipped byte.
// It execs the same build.sh/verify.sh an operator runs, so the gate covers the real scripts.
package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tarMembers returns the set of top-level member names in a .tar.gz.
func tarMembers(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip open %s: %v", path, err)
	}
	tr := tar.NewReader(gz)
	members := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read %s: %v", path, err)
		}
		members[hdr.Name] = true
	}
	return members
}

// build runs build.sh into out (ARCH=amd64 for a stable, deterministic artifact) and returns the
// tarball's basename. A missing tool (go/openssl) fails the gate — they are repo build deps.
func build(t *testing.T, out string) string {
	t.Helper()
	cmd := exec.Command("/usr/bin/env", "bash", "build.sh")
	cmd.Env = append(os.Environ(), "OUT="+out, "ARCH=amd64")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build.sh: %v\n%s", err, stderr.String())
	}
	name := strings.TrimSpace(stdout.String())
	if name == "" {
		t.Fatalf("build.sh emitted no tarball name\nstderr:\n%s", stderr.String())
	}
	return name
}

func sha256File(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// verify runs verify.sh against tarball; ok reports whether it exited zero.
func verify(t *testing.T, tarball string) (ok bool, output string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "verify.sh", tarball)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func TestPackageBuildsVerifiesAndIsDeterministic(t *testing.T) {
	out1 := t.TempDir()
	name := build(t, out1)
	tarball1 := filepath.Join(out1, name)

	// The tarball carries exactly the host-package members.
	members := tarMembers(t, tarball1)
	for _, want := range []string{"palai-runner", "palai-runner.service", "palai-runner.sh", "runner.env.example", "runner-host.md"} {
		if !members[want] {
			t.Fatalf("tarball missing member %q (has %v)", want, members)
		}
	}

	// verify.sh accepts the freshly signed package.
	if ok, out := verify(t, tarball1); !ok {
		t.Fatalf("verify.sh rejected a freshly built package:\n%s", out)
	}

	// Deterministic: a second build of the same source is byte-identical.
	out2 := t.TempDir()
	name2 := build(t, out2)
	if name2 != name {
		t.Fatalf("tarball name changed across builds: %q vs %q", name, name2)
	}
	if a, b := sha256File(t, tarball1), sha256File(t, filepath.Join(out2, name2)); a != b {
		t.Fatalf("tarball is not deterministic: %s != %s", a, b)
	}
}

func TestVerifyFailsOnTamperedTarball(t *testing.T) {
	out := t.TempDir()
	name := build(t, out)
	tarball := filepath.Join(out, name)

	// Sanity: the untampered package verifies.
	if ok, o := verify(t, tarball); !ok {
		t.Fatalf("baseline verify failed:\n%s", o)
	}

	// Flip a single byte in the tarball body, leaving the manifest, signature, and public key
	// untouched — exactly the download-corruption / tamper case verify.sh must catch.
	b, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(tarball, b, 0o600); err != nil {
		t.Fatal(err)
	}

	if ok, o := verify(t, tarball); ok {
		t.Fatalf("verify.sh PASSED a tampered tarball — it must fail closed:\n%s", o)
	}
}
