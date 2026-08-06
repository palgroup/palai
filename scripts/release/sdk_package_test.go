// E16 T7 — the SDK provenance gate (no npm/uv/network): build the signed SDK bundle with
// PALAI_SDK_PACKAGES=go (the hermetic package — go+tar only, no external toolchain), then prove the
// same checksum/sign/verify machinery the operator runs. It execs the SAME sdk-package.sh +
// sdk-verify.sh, asserts the signer is the E14 T5 tool VERBATIM (byte-identical runner-verify.sh),
// and that a tampered artifact FAILS closed. The full three-package build (npm pack + uv build) is a
// hand/CI gate, not this unit test — this proves the provenance discipline, not the package contents.
//
// Honest naming is pinned here too. Since E18 T2 that means: `provenance` names the build-input
// record + the openssl-signed root this script really produces (only `attestation` stays null, and
// the note says why), `sbom` is a real block or an EXPLAINED null, the manifest SAYS publish is
// E18, and NO publish step exists.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const goPackage = "packages/palai-go-sdk-0.1.0-src.tar.gz"

var pristineBundle string

// pristineRelease is one scripts/release/build.sh output (E18 T3's provenance_test.go stages copies
// of it). Built ONCE here because every provenance case needs a real, complete release dir and the
// build is the slow part; the cases copy it so each tampers its own tree.
var pristineRelease string

func TestMain(m *testing.M) {
	root := mustRoot()
	out, err := os.MkdirTemp("", "sdk-bundle-pristine")
	if err != nil {
		panic(err)
	}
	cmd := exec.Command("/usr/bin/env", "bash", filepath.Join(root, "scripts/release/sdk-package.sh"))
	cmd.Env = append(os.Environ(), "OUT="+out, "PALAI_SDK_PACKAGES=go")
	if combined, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(out)
		panic("sdk-package.sh: " + err.Error() + "\n" + string(combined))
	}
	pristineBundle = out

	rel, err := os.MkdirTemp("", "release-pristine")
	if err != nil {
		panic(err)
	}
	build := exec.Command("/usr/bin/env", append([]string{"bash",
		filepath.Join(root, "scripts/release/build.sh"),
		"--out", rel, "--no-images", "--version", "18.0.0"}, reducedMatrix...)...)
	build.Dir = root
	build.Env = append(os.Environ(), scratchBuild...)
	if combined, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(out)
		os.RemoveAll(rel)
		panic("build.sh: " + err.Error() + "\n" + string(combined))
	}
	pristineRelease = rel

	code := m.Run()
	os.RemoveAll(out)
	os.RemoveAll(rel)
	os.Exit(code)
}

func mustRoot() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		panic("git rev-parse: " + err.Error())
	}
	return strings.TrimSpace(string(out))
}

func repoRoot(t *testing.T) string { t.Helper(); return mustRoot() }

// buildBundle returns a fresh, isolated copy of the pristine bundle (cheap cp -R) so a tamper case
// mutates its own copy.
func buildBundle(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	if err := exec.Command("cp", "-R", pristineBundle+"/.", out).Run(); err != nil {
		t.Fatalf("copy pristine bundle: %v", err)
	}
	return out
}

// verify runs the GIT-TRACKED scripts/release/sdk-verify.sh (the natural repo workflow) against the
// bundle. That resolves its sibling runner-verify.sh to the version-controlled
// scripts/release/runner-verify.sh — NOT the bundle's swappable copy — so the signature check is
// fail-closed-secure by default (the SF-1 fix).
func verify(t *testing.T, bundle, pubkey string) (ok bool, output string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", filepath.Join(repoRoot(t), "scripts/release/sdk-verify.sh"), bundle, pubkey)
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
	pub := filepath.Join(bundle, "palai-sdk-signing.pub")

	for _, want := range []string{
		"manifest.json", "build-input.json", "sha256sums", "sha256sums.sig", "sha256sums.sha256",
		"palai-sdk-signing.pub", "runner-verify.sh", "sdk-verify.sh", goPackage,
	} {
		if _, err := os.Stat(filepath.Join(bundle, want)); err != nil {
			t.Fatalf("bundle missing %q: %v", want, err)
		}
	}

	// ONE signer, VERBATIM: the bundled runner-verify.sh is byte-identical to E14 T5's verify.sh.
	if a, b := sha256File(t, filepath.Join(bundle, "runner-verify.sh")),
		sha256File(t, filepath.Join(repoRoot(t), "scripts/package/runner/verify.sh")); a != b {
		t.Fatalf("runner-verify.sh is not the E14 T5 verifier verbatim (%s != %s)", a, b)
	}

	// Honest naming. E18 T2 retired the "sbom/provenance are intentionally null" placeholders:
	// `provenance` now names the record this script really produces, and `sbom` is either a real
	// block (generator available) or null WITH a note saying an absent SBOM is not a clean result.
	// Neither may be a bare null with no explanation, which is what the E16 T7 shape allowed.
	var man map[string]any
	b, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatalf("manifest.json is not valid JSON: %v", err)
	}
	prov, ok := man["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("manifest.provenance must name the build-input record + signed root (got %v)", man["provenance"])
	}
	for _, f := range []string{"build_input", "signed_root", "signature", "signer"} {
		if prov[f] == nil {
			t.Fatalf("manifest.provenance.%s is missing", f)
		}
	}
	if prov["attestation"] != nil {
		t.Fatalf("manifest.provenance.attestation must stay null here — the in-toto statement is "+
			"provenance.sh's, over a release dir (got %v)", prov["attestation"])
	}
	if s, _ := man["provenance_note"].(string); !strings.Contains(s, "provenance.sh") ||
		!strings.Contains(s, "No Sigstore") {
		t.Fatalf("provenance_note must say where the attestation lives and claim no Sigstore (got %q)", s)
	}
	sbomNote, _ := man["sbom_note"].(string)
	switch sbom := man["sbom"].(type) {
	case map[string]any:
		formats, _ := sbom["formats"].([]any)
		if len(formats) != 2 {
			t.Fatalf("manifest.sbom must name both formats, got %v", sbom["formats"])
		}
		if !strings.Contains(sbomNote, "--verify") {
			t.Fatalf("sbom_note must say how to re-check the SBOMs offline (got %q)", sbomNote)
		}
	case nil:
		// The generator could not run. That must READ as an absence, never as a clean result.
		if !strings.Contains(sbomNote, "not a clean result") {
			t.Fatalf("a null sbom must be explained as an absence, not left bare (got %q)", sbomNote)
		}
	default:
		t.Fatalf("manifest.sbom is neither a block nor null: %T", sbom)
	}
	if s, _ := man["note"].(string); !strings.Contains(s, "E18") {
		t.Fatalf("manifest.note must name E18 as where publish lives (got %q)", s)
	}
	// The go package is recorded with a matching sha256 (provenance = the built artifact's digest).
	pkgs, _ := man["packages"].([]any)
	var found bool
	for _, p := range pkgs {
		pm, _ := p.(map[string]any)
		if pm["file"] == goPackage {
			found = true
			if sum, _ := pm["sha256"].(string); sum != sha256File(t, filepath.Join(bundle, goPackage)) {
				t.Fatalf("manifest sha256 for %s does not match the artifact", goPackage)
			}
		}
	}
	if !found {
		t.Fatalf("manifest.packages does not list %s", goPackage)
	}

	// build-input records the git ref + toolchains (the provenance "what was built from").
	var bi map[string]any
	rb, err := os.ReadFile(filepath.Join(bundle, "build-input.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rb, &bi); err != nil {
		t.Fatalf("build-input.json invalid: %v", err)
	}
	if c, _ := bi["git_commit"].(string); c == "" || c == "unknown" {
		t.Fatalf("build-input.git_commit missing (got %q)", bi["git_commit"])
	}
	if _, ok := bi["toolchains"].(map[string]any); !ok {
		t.Fatal("build-input.toolchains missing")
	}

	// sdk-verify.sh accepts the freshly signed bundle against its (out-of-band) key.
	if ok, out := verify(t, bundle, pub); !ok {
		t.Fatalf("sdk-verify.sh rejected a freshly built bundle:\n%s", out)
	}
}

// TestVerifyFailsOnTamperedPackage: flip a byte in a listed artifact (the go source tar). The digest
// chain (sha256sum -c) must catch it — verify FAILS closed. This is the T7 "tamper → FAIL" proof.
func TestVerifyFailsOnTamperedPackage(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-sdk-signing.pub")
	if ok, o := verify(t, bundle, pub); !ok {
		t.Fatalf("baseline verify failed:\n%s", o)
	}
	flipByte(t, filepath.Join(bundle, goPackage))
	if ok, o := verify(t, bundle, pub); ok {
		t.Fatalf("sdk-verify.sh PASSED a tampered package — it must fail closed:\n%s", o)
	}
}

// TestVerifyRejectsReshaTamper: flip a byte in the signed root AND regenerate sha256sums.sha256 to
// match. The digest now agrees, so ONLY the signature can catch it — this turns RED if the openssl
// signature check is removed (the E14 T5 S5a case, at the SDK-bundle level).
func TestVerifyRejectsReshaTamper(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-sdk-signing.pub")

	sums := filepath.Join(bundle, "sha256sums")
	flipByte(t, sums)
	manifest := filepath.Join(bundle, "sha256sums.sha256")
	if err := os.WriteFile(manifest, []byte(sha256File(t, sums)+"  sha256sums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, o := verify(t, bundle, pub); ok {
		t.Fatalf("sdk-verify.sh PASSED a re-sha'd tampered root — the signature must catch it:\n%s", o)
	}
}

// TestVerifyRejectsATruncatedRiderName: an unsigned file whose path is a strict PREFIX of a signed
// path. `sha256sum -c` never looks at it (it is not listed), so the no-unsigned-rider check is the
// only thing that can catch it — and an UNANCHORED `grep -F "  $f"` finds the prefix inside the
// longer signed line and waves the file through. Anchoring the match to a whole line is the fix,
// and this is the case that separates the two.
func TestVerifyRejectsATruncatedRiderName(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-sdk-signing.pub")
	if ok, o := verify(t, bundle, pub); !ok {
		t.Fatalf("baseline verify failed:\n%s", o)
	}
	rider := filepath.Join(bundle, strings.TrimSuffix(goPackage, "z")) // …tar.gz → …tar.g
	if err := os.WriteFile(rider, []byte("unsigned rider\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, o := verify(t, bundle, pub); ok {
		t.Fatalf("sdk-verify.sh ACCEPTED %s — an unsigned file whose name is a prefix of a signed one:\n%s",
			rider, o)
	}
}

// TestVerifyRejectsWrongKey: the signature must bind to a SPECIFIC key — a different P-256 pubkey
// must FAIL, or the signature is just a second checksum.
func TestVerifyRejectsWrongKey(t *testing.T) {
	bundle := buildBundle(t)
	if ok, o := verify(t, bundle, genP256Pubkey(t)); ok {
		t.Fatalf("sdk-verify.sh PASSED against a wrong public key — the signature does not bind:\n%s", o)
	}
}

// TestVerifyRequiresExplicitOutOfBandKey: sdk-verify.sh must NOT default the pubkey to the sibling in
// the bundle dir — that would make the signature a no-op against a fully re-signed channel.
func TestVerifyRequiresExplicitOutOfBandKey(t *testing.T) {
	bundle := buildBundle(t)
	cmd := exec.Command("/bin/sh", filepath.Join(bundle, "sdk-verify.sh"), bundle) // no pubkey arg
	cmd.Env = append(os.Environ(), "PALAI_SDK_PUBKEY=")                            // and none in the env
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("sdk-verify.sh PASSED with no explicit key — it must require an out-of-band key:\n%s", out)
	}
}

// regenChain rebuilds sha256sums (+ its re-sha) over the bundle's CURRENT files — exactly what an
// attacker does after swapping payloads + the bundle's verifier: regenerate the digest chain so it
// passes for THEIR files. The .sig is NOT regenerated (they lack the signing key), so only the
// openssl signature — run by a TRUSTED out-of-band verifier — can catch the swap.
func regenChain(t *testing.T, bundle string) {
	t.Helper()
	const script = `cd "$1" && find . -type f ! -name 'sha256sums' ! -name 'sha256sums.sha256' ` +
		`! -name 'sha256sums.sig' ! -name 'palai-sdk-signing.pub' | LC_ALL=C sort ` +
		`| while IFS= read -r f; do sha256sum "$f"; done > sha256sums && sha256sum sha256sums > sha256sums.sha256`
	if out, err := exec.Command("/bin/sh", "-c", script, "sh", bundle).CombinedOutput(); err != nil {
		t.Fatalf("regen chain: %v\n%s", err, out)
	}
}

// TestTrackedRunnerVerifierIsVerbatim: scripts/release/runner-verify.sh (the sibling the repo
// workflow resolves) must be byte-identical to the E14 T5 verifier, or the one-signer invariant has
// drifted and the fail-closed default would delegate to a stale verifier.
func TestTrackedRunnerVerifierIsVerbatim(t *testing.T) {
	root := repoRoot(t)
	if a, b := sha256File(t, filepath.Join(root, "scripts/release/runner-verify.sh")),
		sha256File(t, filepath.Join(root, "scripts/package/runner/verify.sh")); a != b {
		t.Fatalf("scripts/release/runner-verify.sh is not the E14 T5 verifier verbatim (%s != %s)", a, b)
	}
}

// TestVerifierSwapFailsClosed reproduces the SF-1 exploit: an attacker with NO signing key neuters
// the bundle's runner-verify.sh to `exit 0`, tampers a payload, and regenerates the whole digest
// chain. The signature is the ONLY thing that can catch it. Three postures:
//
//	(a) the git-tracked repo verifier (trusted sibling) → FAILS on the bad signature (the SF-1 fix);
//	(b) the bundle's own verifier, default → FAILS CLOSED (refuses the untrusted bundled verifier);
//	(c) the bundle's own verifier + PALAI_SDK_ALLOW_BUNDLED_VERIFIER=1 → PASSES — proving the exploit
//	    lands ONLY under an explicit local-proof opt-in, i.e. the fail-closed default is what protects.
func TestVerifierSwapFailsClosed(t *testing.T) {
	bundle := buildBundle(t)
	pub := filepath.Join(bundle, "palai-sdk-signing.pub")

	// The attacker: neuter the bundle's verifier, tamper a payload, regenerate the chain (not the sig).
	if err := os.WriteFile(filepath.Join(bundle, "runner-verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	flipByte(t, filepath.Join(bundle, goPackage))
	regenChain(t, bundle)

	// (a) git-tracked repo verifier — its sibling runner-verify.sh is version-controlled, so the real
	// openssl check runs and FAILS on the now-mismatched signature.
	if ok, o := verify(t, bundle, pub); ok {
		t.Fatalf("git-tracked sdk-verify.sh PASSED a verifier-swapped bundle — the SF-1 fix failed:\n%s", o)
	}

	// (b) the bundle's OWN verifier, default (no opt-out) — must fail closed (refuse), not silently
	// trust the neutered bundled verifier.
	bundledDefault := exec.Command("/bin/sh", filepath.Join(bundle, "sdk-verify.sh"), bundle, pub)
	if out, err := bundledDefault.CombinedOutput(); err == nil {
		t.Fatalf("the bundle's own sdk-verify.sh PASSED by default with a neutered verifier — it must fail closed:\n%s", out)
	}

	// (c) the bundle's own verifier WITH the explicit opt-in — passes (the exit-0 verifier + regenerated
	// chain), isolating that the exploit needs the operator to explicitly trust the bundle.
	bundledOptIn := exec.Command("/bin/sh", filepath.Join(bundle, "sdk-verify.sh"), bundle, pub)
	bundledOptIn.Env = append(os.Environ(), "PALAI_SDK_ALLOW_BUNDLED_VERIFIER=1")
	if out, err := bundledOptIn.CombinedOutput(); err != nil {
		t.Fatalf("with PALAI_SDK_ALLOW_BUNDLED_VERIFIER=1 the bundled verifier should run and pass the regenerated chain (isolating the opt-in as the escape hatch):\n%s", out)
	}
}
