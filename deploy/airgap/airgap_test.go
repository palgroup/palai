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

// verify runs the GIT-TRACKED deploy/airgap/verify.sh in host mode (no --network-none, so no Docker)
// and reports whether it exited zero. That is the realistic operator workflow AND the fail-closed one:
// the git-tracked script resolves the ONE signer from the checkout, never from the bundle it is
// verifying (E18 T4; the bundle's own copy now REFUSES unless the caller opts in explicitly).
func verify(t *testing.T, bundle, pubkey string) (ok bool, output string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", filepath.Join(repoRoot(t), "deploy/airgap/verify.sh"), bundle, pubkey)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// verifyWith runs a SPECIFIC verify.sh copy against the bundle and returns its exit code, so a case
// can pin which copy (out-of-band or the bundle's own) refused and with which code.
func verifyWith(t *testing.T, script, bundle, pubkey string, env ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", script, bundle, pubkey)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v\n%s", script, err, out)
	}
	return code, string(out)
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
	if m, _ := filepath.Glob(filepath.Join(bundle, "runner", "palai-*.tar.gz")); len(m) == 0 {
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

// TestVerifierSwapFailsClosed (SF1, closed in E18 T4 — the E16 T7 e4aeb6f pattern applied here).
//
// The exploit: a channel attacker with NO signing key swaps a payload, replaces the bundle's
// runner-verify.sh with `exit 0`, and REGENERATES the whole digest chain over their own files. Every
// listed digest then matches; only the openssl signature over the original sha256sums can catch it —
// and it never runs, because the neutered verifier is the one that would have run it.
//
// Three postures isolate the fix, so the exploit lands ONLY under an explicit trust-the-bundle opt-in:
//
//	(a) the git-tracked deploy/airgap/verify.sh — resolves the ONE signer from the checkout: FAILS (1)
//	(b) the BUNDLE's own verify.sh, no opt-in — REFUSES fail-closed (2), the signature never consulted
//	(c) the BUNDLE's own verify.sh + PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER=1 — PASSES, having warned
//
// Before the fix (b) exited 0: the bundled fallback (`[ -f "$verifier" ] || verifier="$(pwd)/runner-verify.sh"`)
// silently trusted the attacker's copy.
func TestVerifierSwapFailsClosed(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-airgap-signing.pub")

	if err := os.WriteFile(filepath.Join(bundle, "install.sh"), []byte("#!/bin/sh\n# malicious payload\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "runner-verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	regenSums(t, bundle) // the .sig is NOT regenerated (the attacker lacks the key)

	// (a) out of band: the git-tracked script, whose ONE signer comes from the checkout.
	if code, out := verifyWith(t, filepath.Join(repoRoot(t), "deploy/airgap/verify.sh"), bundle, pub); code != 1 {
		t.Fatalf("out-of-band verify.sh exit = %d, want 1 (the bad signature): %s", code, out)
	}
	// (b) the bundle's own copy, no opt-in: fail-CLOSED before the signature is even attempted.
	code, out := verifyWith(t, filepath.Join(bundle, "verify.sh"), bundle, pub)
	if code != 2 {
		t.Fatalf("the BUNDLE's verify.sh exit = %d, want 2 (a refusal). It must not resolve a verifier"+
			" from inside the bundle it verifies:\n%s", code, out)
	}
	if !strings.Contains(out, "REFUSING") {
		t.Errorf("the refusal does not say it is refusing an in-bundle verifier:\n%s", out)
	}
	// (c) the same copy WITH the explicit same-session opt-in: the exploit lands, having warned.
	code, out = verifyWith(t, filepath.Join(bundle, "verify.sh"), bundle, pub,
		"PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER=1")
	if code != 0 {
		t.Fatalf("the explicit opt-in did not work (exit %d):\n%s", code, out)
	}
	if !strings.Contains(out, "WARNING") {
		t.Errorf("the opt-in did not warn that a bundled verifier is not channel-attack safe:\n%s", out)
	}
}

// TestVerifierResolutionRefusesWithNoOutOfBandSigner is the other half of the fail-closed rule: a
// verify.sh with NO out-of-band signer reachable at all (no sibling runner-verify.sh, no checkout two
// levels up) must REFUSE rather than reach into the bundle — even when the bundle is perfectly clean.
// A clean bundle is the harder case: there is no tamper to catch, so only the resolution rule can fail
// this, and a fallback re-introduced later turns it RED.
func TestVerifierResolutionRefusesWithNoOutOfBandSigner(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-airgap-signing.pub")

	// A verify.sh alone in a directory that is NOT a checkout: nothing out of band to resolve.
	orphan := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(orphan, "verify.sh")
	copyFile(t, filepath.Join(repoRoot(t), "deploy/airgap/verify.sh"), script)

	code, out := verifyWith(t, script, bundle, pub)
	if code != 2 {
		t.Fatalf("verify.sh with no out-of-band signer exit = %d, want 2 (a refusal) even on a CLEAN"+
			" bundle:\n%s", code, out)
	}
	if code, out := verifyWith(t, script, bundle, pub, "PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER=1"); code != 0 {
		t.Fatalf("the same-session opt-in must still verify a clean bundle (exit %d):\n%s", code, out)
	}
}
