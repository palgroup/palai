// E18 T3 — the provenance + signing gate. It runs the REAL scripts/release/provenance.sh over a
// REAL scripts/release/build.sh output and then the REAL scripts/release/provenance-verify.sh, so
// nothing here is a re-implementation of what ships.
//
// The claim under test has three halves:
//
//	BINDING    every subject digest in the in-toto Statement is the artifact's BYTES, recomputed
//	           HERE (§2 recompute-over-copy) — never the digest the index or the statement claims.
//	MATERIALS  the resolved dependencies carry the git commit AND every base-image digest of every
//	           release Dockerfile, recomputed HERE by parsing those Dockerfiles.
//	SIGNATURE  one signing tool: the E14 T5 openssl P-256 verifier VERBATIM over a sha256sums root
//	           that binds the index + the statement (+ the SBOM slot T2 fills).
//
// The three tamper negatives are deliberately RE-SIGNED with the same key before verifying. A
// tamper that only breaks the digest chain proves nothing about the binding or the materials leg —
// `sha256sum -c` would catch it first and those legs would never run. Re-signing models the case
// the legs actually exist for: an attestation that is perfectly signed and internally consistent
// but does not describe the bytes (a builder that copied a digest from a log instead of recomputing
// it, or that dropped an input).
package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	statementFile = "provenance.intoto.json"
	// The builder identity this session can honestly claim (plan §T3): NOT a CI workflow identity.
	honestBuilderID = "local-macos-session"
)

// The Dockerfiles scripts/release/build.sh actually builds — the provenance materials must name
// every one of them and every base image they pin.
var releaseDockerfiles = []string{
	"deploy/compose/control-plane.Dockerfile",
	"deploy/compose/runner.Dockerfile",
	"engines/reference/Dockerfile",
}

type resourceDescriptor struct {
	URI         string            `json:"uri"`
	Digest      map[string]string `json:"digest"`
	Name        string            `json:"name"`
	Annotations map[string]any    `json:"annotations,omitempty"`
}

type intotoStatement struct {
	Type          string               `json:"_type"`
	Subject       []resourceDescriptor `json:"subject"`
	PredicateType string               `json:"predicateType"`
	Predicate     struct {
		BuildDefinition struct {
			BuildType            string               `json:"buildType"`
			ExternalParameters   map[string]any       `json:"externalParameters"`
			InternalParameters   map[string]any       `json:"internalParameters"`
			ResolvedDependencies []resourceDescriptor `json:"resolvedDependencies"`
		} `json:"buildDefinition"`
		RunDetails struct {
			Builder struct {
				ID      string            `json:"id"`
				Version map[string]string `json:"version"`
			} `json:"builder"`
			Metadata   map[string]any       `json:"metadata"`
			Byproducts []resourceDescriptor `json:"byproducts"`
		} `json:"runDetails"`
	} `json:"predicate"`
}

// stageRelease copies the pristine build.sh output (built once in TestMain) so each case tampers
// its own copy.
func stageRelease(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	if err := exec.Command("cp", "-R", pristineRelease+"/.", out).Run(); err != nil {
		t.Fatalf("copy pristine release dir: %v", err)
	}
	return out
}

// mintKey generates an ephemeral P-256 keypair OUTSIDE the release dir (a signing key never lives
// beside the artifacts it signs) and returns (key, pub).
func mintKey(t *testing.T) (string, string) {
	t.Helper()
	d := t.TempDir()
	key, pub := filepath.Join(d, "signing.key"), filepath.Join(d, "signing.pub")
	for _, args := range [][]string{
		{"genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256", "-out", key},
		{"pkey", "-in", key, "-pubout", "-out", pub},
	} {
		if out, err := exec.Command("openssl", args...).CombinedOutput(); err != nil {
			t.Fatalf("openssl %v: %v\n%s", args, err, out)
		}
	}
	return key, pub
}

// attest runs the REAL provenance.sh over a staged release dir with a test-held signing key.
func attest(t *testing.T, dir, key string) {
	t.Helper()
	cmd := exec.Command("/usr/bin/env", "bash",
		filepath.Join(repoRoot(t), "scripts/release/provenance.sh"), "--dir", dir, "--key", key)
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provenance.sh: %v\n%s", err, out)
	}
}

// verifyProvenance runs the GIT-TRACKED verifier (so its sibling runner-verify.sh is the
// version-controlled one — fail-closed by default, E16 T7 pattern).
func verifyProvenance(t *testing.T, dir, pub string, env ...string) (bool, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", filepath.Join(repoRoot(t), "scripts/release/provenance-verify.sh"), dir, pub)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// resign regenerates the signed root over whatever the dir now contains, with the SAME key. This is
// the "internally consistent but wrong" attacker/buggy-builder model the binding and materials legs
// exist for.
func resign(t *testing.T, dir, key string) {
	t.Helper()
	script := `set -eu
cd "$1"
find . -type f ! -name sha256sums ! -name sha256sums.sha256 ! -name sha256sums.sig \
  ! -name palai-release-signing.pub \
  | LC_ALL=C sort | while IFS= read -r f; do sha256sum "$f"; done > sha256sums
sha256sum sha256sums > sha256sums.sha256
openssl dgst -sha256 -sign "$2" -out sha256sums.sig sha256sums`
	if out, err := exec.Command("/bin/sh", "-c", script, "resign", dir, key).CombinedOutput(); err != nil {
		t.Fatalf("resign: %v\n%s", err, out)
	}
}

func readStatement(t *testing.T, dir string) intotoStatement {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, statementFile))
	if err != nil {
		t.Fatalf("no %s: %v", statementFile, err)
	}
	var st intotoStatement
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("%s is not valid JSON: %v", statementFile, err)
	}
	return st
}

func readIndex(t *testing.T, dir string) releaseIndex {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "release-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx releaseIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatalf("release-index.json is not valid JSON after attestation: %v\n%s", err, b)
	}
	return idx
}

// rewriteStatement applies fn to the raw statement JSON and writes it back.
func rewriteStatement(t *testing.T, dir string, fn func(map[string]any)) {
	t.Helper()
	p := filepath.Join(dir, statementFile)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	fn(raw)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// baseImageDigests parses one Dockerfile's FROM lines HERE (the canonical source), so the materials
// assertion is a recomputation and not a read-back of what the attestation chose to record.
var fromDigest = regexp.MustCompile(`(?m)^FROM\s+.*@sha256:([0-9a-f]{64})`)

func baseImageDigests(t *testing.T, dockerfile string) []string {
	t.Helper()
	b, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range fromDigest.FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s has no digest-pinned FROM — the materials assertion would be vacuous", dockerfile)
	}
	return out
}

// TestProvenanceBindsRecomputedSubjectsAndMaterials is the crown: shape, binding, materials, and a
// clean verify.
func TestProvenanceBindsRecomputedSubjectsAndMaterials(t *testing.T) {
	dir := stageRelease(t)
	key, pub := mintKey(t)
	attest(t, dir, key)

	st := readStatement(t, dir)
	if st.Type != "https://in-toto.io/Statement/v1" {
		t.Errorf("_type = %q, want the in-toto Statement v1 URI", st.Type)
	}
	if st.PredicateType != "https://slsa.dev/provenance/v1" {
		t.Errorf("predicateType = %q, want the SLSA provenance v1 URI", st.PredicateType)
	}
	if got := st.Predicate.RunDetails.Builder.ID; got != honestBuilderID {
		t.Errorf("builder.id = %q, want the honest %q (a CI workflow identity is plan §6 leg 1)", got, honestBuilderID)
	}
	// The CI workflow-identity slot must EXIST and be empty — T5's real run fills it (§6).
	ip := st.Predicate.BuildDefinition.InternalParameters
	if v, ok := ip["ci_workflow_identity"]; !ok || v != nil {
		t.Errorf("internalParameters.ci_workflow_identity must be present and null until a real CI run (got %v, present=%v)", v, ok)
	}

	// BINDING — every indexed artifact is a subject whose digest is the file's bytes, hashed here.
	idx := readIndex(t, dir)
	if len(idx.Artifacts) == 0 {
		t.Fatal("the release index lists no artifacts — the binding claim would be vacuous")
	}
	subjects := map[string]string{}
	for _, s := range st.Subject {
		if _, dup := subjects[s.Name]; dup {
			t.Errorf("subject %q listed twice", s.Name)
		}
		subjects[s.Name] = s.Digest["sha256"]
	}
	for _, a := range idx.Artifacts {
		got, ok := subjects[a.File]
		if !ok {
			t.Errorf("artifact %s is not a subject of the attestation", a.File)
			continue
		}
		if want := sha256File(t, filepath.Join(dir, a.File)); got != want {
			t.Errorf("subject %s: statement says %s, the bytes are %s", a.File, got, want)
		}
		delete(subjects, a.File)
		if a.Provenance == nil || *a.Provenance != statementFile {
			t.Errorf("index artifact %s: provenance slot = %v, want %q", a.File, a.Provenance, statementFile)
		}
	}
	for extra := range subjects {
		t.Errorf("statement claims subject %q which the release index does not list", extra)
	}
	if idx.Provenance == nil || *idx.Provenance != statementFile {
		t.Errorf("release-index.provenance = %v, want %q", idx.Provenance, statementFile)
	}
	if idx.SBOM != nil {
		t.Errorf("release-index.sbom must stay EMPTY here — it is T2's slot (got %q)", *idx.SBOM)
	}

	// MATERIALS — git commit + every base-image digest of every release Dockerfile.
	materials := map[string]bool{}
	for _, d := range st.Predicate.BuildDefinition.ResolvedDependencies {
		for _, v := range d.Digest {
			materials[v] = true
		}
	}
	commit := strings.TrimSpace(mustOutput(t, "git", "rev-parse", "HEAD"))
	if !materials[commit] {
		t.Errorf("materials do not carry the source commit %s", commit)
	}
	for _, df := range releaseDockerfiles {
		abs := filepath.Join(repoRoot(t), df)
		if !materials[sha256File(t, abs)] {
			t.Errorf("materials do not carry %s's own file digest", df)
		}
		for _, base := range baseImageDigests(t, abs) {
			if !materials[base] {
				t.Errorf("materials do not carry %s's base image digest sha256:%s", df, base)
			}
		}
	}

	// SIGNATURE — one signing tool: the shipped verifier is the E14 T5 one, byte for byte.
	if a, b := sha256File(t, filepath.Join(dir, "runner-verify.sh")),
		sha256File(t, filepath.Join(repoRoot(t), "scripts/package/runner/verify.sh")); a != b {
		t.Errorf("the staged runner-verify.sh is not the E14 T5 verifier VERBATIM (%s != %s)", a, b)
	}
	if ok, out := verifyProvenance(t, dir, pub); !ok {
		t.Fatalf("provenance-verify.sh rejected a freshly attested release:\n%s", out)
	}
	// A signing key must NEVER be left in the release dir (the PEM header is matched whole; the
	// released Go binaries legitimately contain the words "PRIVATE KEY" from crypto/x509).
	if out := mustOutput(t, "grep", "-rl", "-----BEGIN PRIVATE KEY-----", dir); strings.TrimSpace(out) != "" {
		t.Errorf("the release dir contains a private key:\n%s", out)
	}
	if out := mustOutput(t, "find", dir, "-name", "*.key"); strings.TrimSpace(out) != "" {
		t.Errorf("the release dir contains a key file:\n%s", out)
	}
}

// mustOutput runs a command and returns stdout, tolerating grep's "no match" exit 1.
func mustOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil && name != "grep" {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(out)
}

// TestProvenanceRejectsTamperedSubjectDigest — RED-first negative 1. The statement is edited and
// RE-SIGNED, so the signature and the digest chain both pass; only the binding leg can catch it.
func TestProvenanceRejectsTamperedSubjectDigest(t *testing.T) {
	dir := stageRelease(t)
	key, pub := mintKey(t)
	attest(t, dir, key)
	if ok, out := verifyProvenance(t, dir, pub); !ok {
		t.Fatalf("precondition: the clean release must verify:\n%s", out)
	}

	rewriteStatement(t, dir, func(raw map[string]any) {
		subs := raw["subject"].([]any)
		d := subs[0].(map[string]any)["digest"].(map[string]any)
		d["sha256"] = strings.Repeat("0", 64)
	})
	resign(t, dir, key)

	ok, out := verifyProvenance(t, dir, pub)
	if ok {
		t.Fatalf("a re-signed attestation with a WRONG subject digest verified:\n%s", out)
	}
	if !strings.Contains(out, "subject") {
		t.Errorf("the failure does not name the subject binding — it may have failed for another reason:\n%s", out)
	}
}

// TestProvenanceRejectsDroppedBaseImageMaterial — RED-first negative 2. Same re-sign discipline:
// only a materials leg that RECOMPUTES the expected set from the Dockerfiles can catch a drop.
func TestProvenanceRejectsDroppedBaseImageMaterial(t *testing.T) {
	dir := stageRelease(t)
	key, pub := mintKey(t)
	attest(t, dir, key)

	want := baseImageDigests(t, filepath.Join(repoRoot(t), releaseDockerfiles[0]))[0]
	var dropped bool
	rewriteStatement(t, dir, func(raw map[string]any) {
		pred := raw["predicate"].(map[string]any)["buildDefinition"].(map[string]any)
		deps := pred["resolvedDependencies"].([]any)
		var kept []any
		for _, d := range deps {
			dm := d.(map[string]any)
			dig, _ := dm["digest"].(map[string]any)
			if s, _ := dig["sha256"].(string); s == want {
				dropped = true
				continue
			}
			kept = append(kept, d)
		}
		pred["resolvedDependencies"] = kept
	})
	if !dropped {
		t.Fatalf("no material carried base image sha256:%s — nothing was dropped, the negative is vacuous", want)
	}
	resign(t, dir, key)

	ok, out := verifyProvenance(t, dir, pub)
	if ok {
		t.Fatalf("a re-signed attestation MISSING a base-image material verified:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "material") {
		t.Errorf("the failure does not name the materials leg:\n%s", out)
	}
}

// TestProvenanceRejectsWrongAndMissingKey — RED-first negative 3. The E14 T5 trust model, unchanged:
// the key comes from OUT OF BAND, a wrong one fails and a missing one refuses.
func TestProvenanceRejectsWrongAndMissingKey(t *testing.T) {
	dir := stageRelease(t)
	key, pub := mintKey(t)
	attest(t, dir, key)
	if ok, out := verifyProvenance(t, dir, pub); !ok {
		t.Fatalf("precondition: the clean release must verify:\n%s", out)
	}

	_, otherPub := mintKey(t)
	if ok, out := verifyProvenance(t, dir, otherPub); ok {
		t.Fatalf("verification passed against the WRONG public key:\n%s", out)
	}

	cmd := exec.Command("/bin/sh", filepath.Join(repoRoot(t), "scripts/release/provenance-verify.sh"), dir)
	cmd.Env = append(os.Environ(), "PALAI_RELEASE_PUBKEY=")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("verification passed with NO public key at all:\n%s", out)
	}
}

// TestProvenanceRejectsTamperedArtifactBytes — the digest-chain half: change a released byte and
// the signed root no longer describes the tree.
func TestProvenanceRejectsTamperedArtifactBytes(t *testing.T) {
	dir := stageRelease(t)
	key, pub := mintKey(t)
	attest(t, dir, key)

	idx := readIndex(t, dir)
	flipByte(t, filepath.Join(dir, idx.Artifacts[0].File))
	if ok, out := verifyProvenance(t, dir, pub); ok {
		t.Fatalf("verification passed over a tampered artifact:\n%s", out)
	}
}

// TestProvenanceVerifierResolutionIsFailClosed — §2: a verifier is never resolved from inside the
// thing it verifies. Running the release dir's OWN copy must REFUSE, and the bundled verifier is an
// explicit env opt-in for a same-session local proof only (the E16 T7 e4aeb6f pattern).
func TestProvenanceVerifierResolutionIsFailClosed(t *testing.T) {
	dir := stageRelease(t)
	key, pub := mintKey(t)
	attest(t, dir, key)

	run := func(env ...string) (bool, string) {
		cmd := exec.Command("/bin/sh", filepath.Join(dir, "provenance-verify.sh"), dir, pub)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return err == nil, string(out)
	}
	ok, out := run()
	if ok {
		t.Fatalf("the release dir's OWN verifier ran without the opt-in (fail-OPEN):\n%s", out)
	}
	if !strings.Contains(out, "REFUS") {
		t.Errorf("the refusal does not say it is refusing an untrusted in-bundle verifier:\n%s", out)
	}
	if ok, out := run("PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER=1", "PALAI_RELEASE_SOURCE_ROOT="+repoRoot(t)); !ok {
		t.Fatalf("the explicit same-session opt-in did not work:\n%s", out)
	}
}

// TestProvenanceUnsourcedMaterialsAreFailClosed — the second fail-closed knob: without a source tree
// the materials leg cannot RECOMPUTE anything, so it refuses rather than silently skipping. The
// degraded, signature-only mode is an explicit opt-in that says so out loud.
func TestProvenanceUnsourcedMaterialsAreFailClosed(t *testing.T) {
	dir := stageRelease(t)
	key, pub := mintKey(t)
	attest(t, dir, key)

	// Running the release dir's own copy is the "shipped without the source tree" shape.
	run := func(env ...string) (bool, string) {
		cmd := exec.Command("/bin/sh", filepath.Join(dir, "provenance-verify.sh"), dir, pub)
		cmd.Env = append(os.Environ(), append([]string{"PALAI_RELEASE_ALLOW_BUNDLED_VERIFIER=1"}, env...)...)
		out, err := cmd.CombinedOutput()
		return err == nil, string(out)
	}
	ok, out := run()
	if ok {
		t.Fatalf("materials verification silently passed with no source tree to recompute from:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "material") {
		t.Errorf("the refusal does not name the materials leg:\n%s", out)
	}
	ok, out = run("PALAI_RELEASE_ALLOW_UNSOURCED_MATERIALS=1")
	if !ok {
		t.Fatalf("the explicit degraded-materials opt-in did not work:\n%s", out)
	}
	if !strings.Contains(out, "WARNING") {
		t.Errorf("the degraded mode did not warn that leg (4) was reduced to signature-only:\n%s", out)
	}
}

// TestProvenanceHonestNaming — §2 honest naming, mechanically. The attestation is SLSA-SHAPED and
// signed with openssl; it may not imply Sigstore/Fulcio/Rekor or a bare "SLSA L2", and the one place
// those words may appear is the explicit ceiling that DISCLAIMS them.
func TestProvenanceHonestNaming(t *testing.T) {
	dir := stageRelease(t)
	key, _ := mintKey(t)
	attest(t, dir, key)

	raw, err := os.ReadFile(filepath.Join(dir, statementFile))
	if err != nil {
		t.Fatal(err)
	}
	st := readStatement(t, dir)
	ceiling, _ := st.Predicate.BuildDefinition.InternalParameters["honest_ceiling"].(string)
	if ceiling == "" {
		t.Fatal("the statement carries no internalParameters.honest_ceiling")
	}
	for _, want := range []string{"openssl", "L2-shaped", honestBuilderID} {
		if !strings.Contains(ceiling, want) {
			t.Errorf("honest_ceiling must name %q; got: %s", want, ceiling)
		}
	}

	// Strip the disclaimer, then the REST of the attestation may not name any of these.
	rest := strings.ToLower(strings.ReplaceAll(string(raw), ceiling, ""))
	for _, banned := range []string{"cosign", "sigstore", "fulcio", "rekor", "transparency log", "slsa l2", "slsa level 2"} {
		if strings.Contains(rest, banned) {
			t.Errorf("the attestation claims %q outside the honest ceiling", banned)
		}
	}
	// "cosign-verified" is banned outright, everywhere, including the scripts (plan §2).
	for _, f := range []string{"scripts/release/provenance.sh", "scripts/release/provenance-verify.sh"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(b)), "cosign-verified") {
			t.Errorf("%s uses the banned phrase \"cosign-verified\"", f)
		}
	}
}

// TestProvenanceOfflineVerifyNetworkNone proves the verify needs NO network by running it inside a
// container with no network device. It needs an already-loaded openssl+python3 image, so it is
// operator-gated (PALAI_PROVENANCE_TOOL_IMAGE=palai/reference-engine:local go test ...).
func TestProvenanceOfflineVerifyNetworkNone(t *testing.T) {
	image := os.Getenv("PALAI_PROVENANCE_TOOL_IMAGE")
	if image == "" {
		t.Skip("set PALAI_PROVENANCE_TOOL_IMAGE to an already-loaded openssl+python3 image to run the --network none leg")
	}
	dir := stageRelease(t)
	key, pub := mintKey(t)
	attest(t, dir, key)

	cmd := exec.Command("/bin/sh", filepath.Join(repoRoot(t), "scripts/release/provenance-verify.sh"),
		"--network-none", dir, pub, image)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("offline (--network none) verify failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Errorf("offline verify did not report OK:\n%s", out)
	}
}
