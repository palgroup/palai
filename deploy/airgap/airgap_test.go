// E15 T4 — the air-gap bundle gate (no Docker): build the signed bundle with
// PALAI_AIRGAP_IMAGES=skip (staging every part EXCEPT the image tars), then prove the
// sign/verify/digest-chain machinery. It execs the SAME airgap-build.sh + verify.sh an
// operator runs, and asserts the signer is the E14 T5 tool VERBATIM (byte-identical
// runner-verify.sh) — so a second signer or a dropped signature check turns this RED.
// The heavy image `docker save` + internal-network install + real run are the live drill
// (deploy/airgap/drill.sh), not this unit gate.
package airgap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pristineBundle is built ONCE (TestMain) in skip-images mode; each test copies it into a fresh
// temp dir so the tamper cases can mutate an isolated copy without a full rebuild each time.
var pristineBundle string

func TestMain(m *testing.M) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		panic("git rev-parse: " + err.Error())
	}
	out, err := os.MkdirTemp("", "airgap-pristine")
	if err != nil {
		panic(err)
	}
	cmd := exec.Command("/usr/bin/env", "bash", filepath.Join(strings.TrimSpace(string(root)), "scripts/release/airgap-build.sh"))
	cmd.Env = append(os.Environ(), "OUT="+out, "ARCH=amd64", "PALAI_AIRGAP_IMAGES=skip")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(out)
		panic("airgap-build.sh: " + err.Error() + "\n" + stderr.String())
	}
	pristineBundle = out
	code := m.Run()
	os.RemoveAll(out)
	os.Exit(code)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// buildBundle returns a fresh, isolated copy of the pristine bundle (cheap cp -R).
func buildBundle(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	if err := exec.Command("cp", "-R", pristineBundle+"/.", out).Run(); err != nil {
		t.Fatalf("copy pristine bundle: %v", err)
	}
	return out
}

// verify runs the bundle's verify.sh in host mode (no --network-none, so no Docker) and reports
// whether it exited zero.
func verify(t *testing.T, bundle, pubkey string) (ok bool, output string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", filepath.Join(bundle, "verify.sh"), bundle, pubkey)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
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

// genP256Pubkey mints a throwaway ECDSA P-256 keypair (a DIFFERENT signer) and returns its pubkey.
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

func TestBundleBuildsAndVerifies(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-airgap-signing.pub")

	// The bundle carries every §45.9 component (image tars are skipped in this unit gate).
	for _, want := range []string{
		"manifest.json", "sha256sums", "sha256sums.sig", "sha256sums.sha256",
		"palai-airgap-signing.pub", "runner-verify.sh", "verify.sh", "install.sh", "airgap.yml",
		"bin/palai-linux-amd64", "compose/compose.yaml", "helm/palai/Chart.yaml",
		"migrations",
	} {
		if _, err := os.Stat(filepath.Join(bundle, want)); err != nil {
			t.Fatalf("bundle missing %q: %v", want, err)
		}
	}
	// The runner host package (E14 T5 tarball) is inside runner/.
	if m, _ := filepath.Glob(filepath.Join(bundle, "runner", "palai-runner-host-*.tar.gz")); len(m) == 0 {
		t.Fatal("bundle missing the E14 runner host package under runner/")
	}

	// ONE signer, VERBATIM: the bundled runner-verify.sh is byte-identical to E14 T5's verify.sh.
	if a, b := sha256File(t, filepath.Join(bundle, "runner-verify.sh")),
		sha256File(t, filepath.Join(repoRoot(t), "scripts/package/runner/verify.sh")); a != b {
		t.Fatalf("runner-verify.sh is not the E14 T5 verifier verbatim (%s != %s)", a, b)
	}

	// Honest naming: SBOM/provenance fields are DEFINED but empty, and the manifest says so.
	var man map[string]any
	b, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatalf("manifest.json is not valid JSON: %v", err)
	}
	for _, f := range []string{"sbom", "provenance"} {
		if v, ok := man[f]; !ok || v != nil {
			t.Fatalf("manifest.%s must be present and null (got %v, present=%v)", f, v, ok)
		}
	}
	for _, f := range []string{"sbom_note", "provenance_note"} {
		if s, _ := man[f].(string); !strings.Contains(s, "E18") {
			t.Fatalf("manifest.%s must name E18 as where production lives (got %q)", f, s)
		}
	}

	// verify.sh accepts the freshly signed bundle against its (out-of-band) key.
	if ok, out := verify(t, bundle, pub); !ok {
		t.Fatalf("verify.sh rejected a freshly built bundle:\n%s", out)
	}
}

// TestVerifyFailsOnTamperedComponent: flip a byte in a listed file (the CLI binary). The digest
// chain (sha256sum -c) must catch it — verify FAILS closed.
func TestVerifyFailsOnTamperedComponent(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-airgap-signing.pub")
	if ok, o := verify(t, bundle, pub); !ok {
		t.Fatalf("baseline verify failed:\n%s", o)
	}
	flipByte(t, filepath.Join(bundle, "bin/palai-linux-amd64"))
	if ok, o := verify(t, bundle, pub); ok {
		t.Fatalf("verify.sh PASSED a tampered component — it must fail closed:\n%s", o)
	}
}

// TestVerifyRejectsReshaTamper: flip a byte in the signed root AND regenerate sha256sums.sha256 to
// match. The digest now agrees, so ONLY the signature can catch it — this turns RED if the openssl
// signature check is removed (the E14 T5 S5a case, at the bundle level).
func TestVerifyRejectsReshaTamper(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-airgap-signing.pub")

	sums := filepath.Join(bundle, "sha256sums")
	flipByte(t, sums)
	// Rewrite sha256sums.sha256 to the tampered root's digest ("<hash>  sha256sums").
	manifest := filepath.Join(bundle, "sha256sums.sha256")
	if err := os.WriteFile(manifest, []byte(sha256File(t, sums)+"  sha256sums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, o := verify(t, bundle, pub); ok {
		t.Fatalf("verify.sh PASSED a re-sha'd tampered root — the signature must catch it:\n%s", o)
	}
}

// TestVerifyRejectsWrongKey: the signature must bind to a SPECIFIC key — a different P-256 pubkey
// must FAIL, or the signature is just a second checksum (the E14 T5 MF1/S5b case).
func TestVerifyRejectsWrongKey(t *testing.T) {
	bundle := buildBundle(t)
	if ok, o := verify(t, bundle, genP256Pubkey(t)); ok {
		t.Fatalf("verify.sh PASSED against a wrong public key — the signature does not bind:\n%s", o)
	}
}

// TestVerifyRequiresExplicitOutOfBandKey: verify.sh must NOT default the pubkey to the sibling in
// the bundle dir — that would make the signature a no-op against a fully re-signed channel.
func TestVerifyRequiresExplicitOutOfBandKey(t *testing.T) {
	bundle := buildBundle(t)
	cmd := exec.Command("/bin/sh", filepath.Join(bundle, "verify.sh"), bundle) // no pubkey arg
	cmd.Env = append(os.Environ(), "PALAI_AIRGAP_PUBKEY=")                     // and none in the env
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("verify.sh PASSED with no explicit key — it must require an out-of-band key:\n%s", out)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// regenSums re-generates sha256sums (+ its re-sha) over the bundle's CURRENT files, exactly as
// airgap-build.sh does — the attacker's move: after swapping payloads + the verifier, they
// regenerate the digest chain so it passes for THEIR files. Only the openssl signature (over the
// original sha256sums) can then catch the change.
func regenSums(t *testing.T, bundle string) {
	t.Helper()
	const script = `cd "$1" && find . -type f ! -name 'sha256sums' ! -name 'sha256sums.sha256' ` +
		`! -name 'sha256sums.sig' ! -name 'palai-airgap-signing.pub' | LC_ALL=C sort ` +
		`| while IFS= read -r f; do sha256sum "$f"; done > sha256sums && sha256sum sha256sums > sha256sums.sha256`
	if out, err := exec.Command("/bin/sh", "-c", script, "sh", bundle).CombinedOutput(); err != nil {
		t.Fatalf("regen sums: %v\n%s", err, out)
	}
}

// TestBuildFailsOnDirtyTree (SF2): a stray untracked file under a staged dir (deploy/compose,
// deploy/helm, storage/migrations) would be silently signed + shipped — the build must refuse.
func TestBuildFailsOnDirtyTree(t *testing.T) {
	root := repoRoot(t)
	stray, err := os.CreateTemp(filepath.Join(root, "deploy/compose"), ".airgap-dirtytest-*")
	if err != nil {
		t.Fatal(err)
	}
	stray.Close()
	t.Cleanup(func() { os.Remove(stray.Name()) })

	out := t.TempDir()
	cmd := exec.Command("/usr/bin/env", "bash", filepath.Join(root, "scripts/release/airgap-build.sh"))
	cmd.Env = append(os.Environ(), "OUT="+out, "ARCH=amd64", "PALAI_AIRGAP_IMAGES=skip")
	combined, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("airgap-build.sh built a DIRTY tree — it must refuse:\n%s", combined)
	}
	if !strings.Contains(string(combined), "DIRTY") {
		t.Fatalf("expected a dirty-tree refusal, got:\n%s", combined)
	}
}

// TestVerifyPrefersOutOfBandVerifier (SF1): if the bundle's runner-verify.sh is swapped for a no-op
// and the sums are re-generated, a bundle-relative verify would pass — but an out-of-band verify.sh
// with the REAL runner-verify.sh beside it must still FAIL closed on the bad signature.
func TestVerifyPrefersOutOfBandVerifier(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-airgap-signing.pub")

	// The channel attacker: swap a payload, neuter the bundle's verifier, and REGENERATE the chain.
	if err := os.WriteFile(filepath.Join(bundle, "install.sh"), []byte("#!/bin/sh\n# malicious payload\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "runner-verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	regenSums(t, bundle) // the .sig is NOT regenerated (attacker lacks the key) — only it can catch this
	// Sanity: with the neutered bundle-relative verifier, verify PASSES — the attack is real.
	if ok, o := verify(t, bundle, pub); !ok {
		t.Skipf("digest chain already caught the tamper; attack not isolated to the signature:\n%s", o)
	}
	// An out-of-band verify.sh + REAL runner-verify.sh sitting together must FAIL closed.
	oob := t.TempDir()
	copyFile(t, filepath.Join(repoRoot(t), "deploy/airgap/verify.sh"), filepath.Join(oob, "verify.sh"))
	copyFile(t, filepath.Join(repoRoot(t), "scripts/package/runner/verify.sh"), filepath.Join(oob, "runner-verify.sh"))
	out, err := exec.Command("/bin/sh", filepath.Join(oob, "verify.sh"), bundle, pub).CombinedOutput()
	if err == nil {
		t.Fatalf("out-of-band verify.sh PASSED a bundle whose verifier was neutered — the OOB verifier must be preferred:\n%s", out)
	}
}
