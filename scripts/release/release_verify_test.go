// E18 T4 — the unified offline release verifier, SEC-101's tamper matrix, and the attestation's
// scan byproducts.
//
// Everything here drives a COMPLETE release directory: scripts/release/build.sh's output, plus the
// sbom/ subtree scripts/release/sbom-tool.py really writes, plus scripts/release/provenance.sh's
// attestation and signed root — then the REAL scripts/release/release-verify.sh over it. Nothing is
// re-implemented; the fixtures are the ones T2 ships (its own committed scanner output) so the SBOM
// and the vulnerability decision are real documents rather than shapes.
//
// CEILINGS of the staging below, stated once:
//   - the SBOMs are T2's committed fixture pair re-used for every artifact (generating a real SBOM per
//     artifact needs Docker + syft; TestSBOMPipelineLive is where that runs);
//   - the image artifact is a docker-save-SHAPED tar, not a runnable image. What the verifier's image
//     leg reads is the tar's manifest.json and the config blob whose sha256 IS the image id, and the
//     fixture carries exactly that. The real six-image matrix is `make release-matrix-smoke`.
//
// Neither ceiling weakens a tamper arm: every arm asserts a DIGEST relationship over bytes that are
// really there, and a fixture byte is as tamperable as a released one.
package release

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- staging a complete release directory ---------------------------------------------------------

func readRawIndex(t *testing.T, dir string) map[string]any {
	t.Helper()
	var doc map[string]any
	readJSON(t, filepath.Join(dir, "release-index.json"), &doc)
	return doc
}

func indexArtifacts(t *testing.T, dir string) []map[string]any {
	t.Helper()
	raw, ok := readRawIndex(t, dir)["artifacts"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatal("the staged release index lists no artifacts")
	}
	out := make([]map[string]any, 0, len(raw))
	for _, a := range raw {
		out = append(out, a.(map[string]any))
	}
	return out
}

// addFixtureImage writes a docker-save-SHAPED image tar into the staged release dir and indexes it,
// so the image legs (image_id recomputed from the config blob, arch from the config blob) have a
// subject at all in a Docker-free run. The tar's own bytes give the artifact digest; the config
// blob's sha256 gives the image id — both recomputed, never chosen.
func addFixtureImage(t *testing.T, dir string) string {
	t.Helper()
	config := []byte(`{"architecture":"arm64","os":"linux","config":{},` +
		`"rootfs":{"type":"layers","diff_ids":[]}}`)
	imageID := "sha256:" + sha256Bytes(config)
	blob := "blobs/sha256/" + strings.TrimPrefix(imageID, "sha256:")
	manifest := []byte(fmt.Sprintf(`[{"Config":%q,"RepoTags":["palai/fixture-engine:t4"],"Layers":[]}]`, blob))

	rel := "images/fixture-engine-linux-arm64.tar"
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(fh)
	for _, m := range []struct {
		name string
		body []byte
	}{{blob, config}, {"manifest.json", manifest}} {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}

	doc := readRawIndex(t, dir)
	doc["artifacts"] = append(doc["artifacts"].([]any), map[string]any{
		"kind": "image", "file": rel, "os": "linux", "arch": "arm64",
		"digest": "sha256:" + sha256File(t, path),
		// `ref` is INFORMATIONAL (build.sh says so): a verifier must resolve by digest/image_id only.
		"ref": "palai/fixture-engine:t4", "image_id": imageID,
		"sbom": nil, "provenance": nil,
	})
	writeJSON(t, filepath.Join(dir, "release-index.json"), doc)
	return rel
}

// rollupSBOMs runs the REAL scripts/release/sbom-tool.py rollup over the staged dir, exactly as
// sbom.sh does after syft/grype have written their documents — so the sbom/ subtree, the license
// inventory, the vulnerability report and the patched index are produced by the shipping code.
func rollupSBOMs(t *testing.T, dir string) {
	t.Helper()
	root := repoRoot(t)
	sbomDir := filepath.Join(dir, "sbom")
	if err := os.MkdirAll(sbomDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var tsv strings.Builder
	for _, art := range indexArtifacts(t, dir) {
		file := art["file"].(string)
		base := strings.ReplaceAll(file, "/", "_")
		for src, suffix := range map[string]string{
			fixtureSPDX: ".spdx.json", fixtureCDX: ".cdx.json", fixtureReport: ".grype.json",
		} {
			b, err := os.ReadFile(filepath.Join(root, src))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sbomDir, base+suffix), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Fprintf(&tsv, "%s\t%s\t%s\n", art["kind"].(string), file, base)
	}
	tsvPath := filepath.Join(t.TempDir(), "artifacts.tsv")
	if err := os.WriteFile(tsvPath, []byte(tsv.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// The fixture report carries real Criticals; the shipped policy blocks them. A live, owner-attributed
	// exception is the documented way past the gate, and it is what a passing release dir looks like.
	policy := filepath.Join(sbomDir, "vuln-policy.json")
	b, err := os.ReadFile(policyWith(t, 1, 30, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, b, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("python3", filepath.Join(root, "scripts/release/sbom-tool.py"), "rollup")
	cmd.Env = append(os.Environ(),
		"DIR="+dir, "MANIFEST=release-index.json", "SDK=0", "TSV="+tsvPath, "SCANNED=1",
		"LOCK="+filepath.Join(root, "scripts/release/vulndb.lock.json"),
		"POLICY="+policy, "SYFT_IMAGE=test", "GRYPE_IMAGE=test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sbom-tool rollup: %v\n%s", err, out)
	}
}

// stageFullRelease is build.sh → sbom → provenance in the order the release chain runs them, minus
// the attestation (each case attests with its own key).
func stageFullRelease(t *testing.T) string {
	t.Helper()
	dir := stageRelease(t)
	addFixtureImage(t, dir)
	rollupSBOMs(t, dir)
	return dir
}

func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- the attestation's scan byproducts --------------------------------------------------------------

// TestProvenanceDeclaresEveryScanByproduct — the attestation must DECLARE everything the scan
// produced, not just the documents whose basename happens to look like an SBOM.
//
// provenance.sh globbed byproducts by BASENAME (`startswith("sbom") or endswith((".spdx.json",
// ".cdx.json"))`). Against T2's real filenames that names the SBOM documents and NOTHING ELSE: not
// vuln-report.json — the file carrying the §51.3 policy DECISION — not the per-artifact scanner
// reports, not the policy copy, not the license inventory. All of them are SIGNED (every file in the
// dir is hashed into the signed root) but never DECLARED, and the run's own count line reported a
// fraction of what it produced. A release-verify that reads the attestation could not see the
// vulnerability decision at all.
func TestProvenanceDeclaresEveryScanByproduct(t *testing.T) {
	dir := stageFullRelease(t)
	key, _ := mintKey(t)
	attest(t, dir, key)

	// The canonical set: every file T2 wrote under sbom/, enumerated from the tree, not from the
	// attestation's own list.
	want := map[string]string{}
	sbomDir := filepath.Join(dir, "sbom")
	entries, err := os.ReadDir(sbomDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		want["sbom/"+e.Name()] = sha256File(t, filepath.Join(sbomDir, e.Name()))
	}
	if len(want) < 4 {
		t.Fatalf("the staged sbom/ has only %d files — the assertion would be vacuous", len(want))
	}

	got := map[string]string{}
	names := map[string]string{}
	for _, b := range readStatement(t, dir).Predicate.RunDetails.Byproducts {
		rel := strings.TrimPrefix(b.URI, "file://")
		got[rel] = b.Digest["sha256"]
		names[rel] = b.Name
	}

	for rel, digest := range want {
		switch d, ok := got[rel]; {
		case !ok:
			t.Errorf("%s is in the release but the attestation declares no byproduct for it", rel)
		case d != digest:
			t.Errorf("byproduct %s: the attestation says sha256:%s, the bytes are sha256:%s", rel, d, digest)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("the attestation declares byproduct %q, which is not in sbom/", rel)
		}
	}

	// ...and each is named for WHAT IT IS. "sbom" for a vulnerability decision is not a name, it is a
	// mislabel a consumer would filter on.
	for rel, want := range map[string]string{
		"sbom/vuln-report.json":       "vuln-decision",
		"sbom/vuln-policy.json":       "vuln-policy",
		"sbom/license-inventory.json": "license-inventory",
	} {
		if got := names[rel]; got != want {
			t.Errorf("byproduct %s is named %q, want %q", rel, got, want)
		}
	}
	for rel, name := range names {
		var want string
		switch {
		case strings.HasSuffix(rel, ".spdx.json"):
			want = "sbom-spdx"
		case strings.HasSuffix(rel, ".cdx.json"):
			want = "sbom-cyclonedx"
		case strings.HasSuffix(rel, ".grype.json"):
			want = "vuln-scan"
		default:
			continue
		}
		if name != want {
			t.Errorf("byproduct %s is named %q, want %q", rel, name, want)
		}
	}

	// The count the run reports about itself must be the truth, or the honest-ceiling text is the lie.
	slot, _ := readStatement(t, dir).Predicate.BuildDefinition.
		InternalParameters["sbom_slot"].(string)
	if !strings.Contains(slot, fmt.Sprintf("found %d.", len(want))) {
		t.Errorf("sbom_slot reports the wrong count (%d files under sbom/):\n%s", len(want), slot)
	}
}
