// E14 T5 — the package gate: build the signed runner host package, prove it verifies, prove it is
// deterministic (byte-identical rebuild, SAME toolchain), and prove verify FAILS closed on a
// flipped byte, a re-signed manifest, a WRONG signing key, and a missing out-of-band key. It execs
// the same build.sh/verify.sh an operator runs, so the gate covers the real scripts — deleting
// verify.sh's signature check would turn the wrong-key and re-sha cases RED.
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

// pubkeyOf is the public key build.sh emitted beside the tarball. In these tests it stands in for
// the operator's OUT-OF-BAND trusted key (same trusted session, no channel attacker); verify.sh
// itself refuses to default to it (TestVerifyRequiresExplicitOutOfBandKey).
func pubkeyOf(out string) string { return filepath.Join(out, "palai-runner-signing.pub") }

func sha256File(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// verify runs verify.sh <tarball> <pubkey>; ok reports whether it exited zero.
func verify(t *testing.T, tarball, pubkey string) (ok bool, output string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "verify.sh", tarball, pubkey)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// genP256Pubkey mints a throwaway ECDSA P-256 keypair (a DIFFERENT signer than build.sh's) and
// returns its public key path — the attacker's key in the wrong-key case.
func genP256Pubkey(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "other.key")
	pub := filepath.Join(dir, "other.pub")
	run := func(args ...string) {
		if out, err := exec.Command("openssl", args...).CombinedOutput(); err != nil {
			t.Fatalf("openssl %v: %v\n%s", args, err, out)
		}
	}
	run("genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256", "-out", key)
	run("pkey", "-in", key, "-pubout", "-out", pub)
	return pub
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

	// verify.sh accepts the freshly signed package against its (out-of-band) key.
	if ok, out := verify(t, tarball1, pubkeyOf(out1)); !ok {
		t.Fatalf("verify.sh rejected a freshly built package:\n%s", out)
	}

	// Deterministic (same toolchain): a second build of the same source is byte-identical.
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

	if ok, o := verify(t, tarball, pubkeyOf(out)); !ok {
		t.Fatalf("baseline verify failed:\n%s", o)
	}

	// Flip a single byte, leaving manifest+sig+key untouched — download-corruption / tamper.
	flipByte(t, tarball)

	if ok, o := verify(t, tarball, pubkeyOf(out)); ok {
		t.Fatalf("verify.sh PASSED a tampered tarball — it must fail closed:\n%s", o)
	}
}

// TestVerifyRejectsReshaTamper (S5a): flip a byte AND regenerate the .sha256 to match the tampered
// tarball. The digest now agrees, so ONLY the signature can catch it — this is the case that turns
// RED if verify.sh's openssl block is removed.
func TestVerifyRejectsReshaTamper(t *testing.T) {
	out := t.TempDir()
	name := build(t, out)
	tarball := filepath.Join(out, name)

	flipByte(t, tarball)
	// Rewrite the manifest to the tampered tarball's digest ("<hash>  <name>", verify reads field 1).
	manifest := tarball + ".sha256"
	if err := os.WriteFile(manifest, []byte(sha256File(t, tarball)+"  "+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if ok, o := verify(t, tarball, pubkeyOf(out)); ok {
		t.Fatalf("verify.sh PASSED a re-sha'd tampered tarball — the signature must catch it:\n%s", o)
	}
}

// TestVerifyRejectsWrongKey (MF1 / S5b): the signature must bind to a SPECIFIC key. An operator
// holding the real out-of-band key rejects a package signed by anyone else — verify against a
// different key's public half must FAIL, or the signature is just a checksum.
func TestVerifyRejectsWrongKey(t *testing.T) {
	out := t.TempDir()
	name := build(t, out)
	tarball := filepath.Join(out, name)

	if ok, o := verify(t, tarball, genP256Pubkey(t)); ok {
		t.Fatalf("verify.sh PASSED against a wrong public key — the signature does not bind to the signer:\n%s", o)
	}
}

// TestVerifyRequiresExplicitOutOfBandKey (MF1): verify.sh must NOT default the pubkey to the sibling
// in the package dir — that made the signature a no-op against a fully re-signed channel. With no
// key argument it must fail closed even though the sibling .pub is right there.
func TestVerifyRequiresExplicitOutOfBandKey(t *testing.T) {
	out := t.TempDir()
	name := build(t, out)
	tarball := filepath.Join(out, name)

	cmd := exec.Command("/bin/sh", "verify.sh", tarball)   // no pubkey arg
	cmd.Env = append(os.Environ(), "PALAI_RUNNER_PUBKEY=") // and none in the env
	out2, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("verify.sh PASSED with no explicit key — it must require an out-of-band key:\n%s", out2)
	}
}

func flipByte(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
