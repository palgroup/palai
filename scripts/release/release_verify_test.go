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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
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
	imageID := writeFixtureImage(t, filepath.Join(dir, fixtureImage), config)
	path := filepath.Join(dir, fixtureImage)
	rel := fixtureImage

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

// fixtureImage is the docker-save-SHAPED image artifact every staged release carries.
const fixtureImage = "images/fixture-engine-linux-arm64.tar"

// writeFixtureImage writes a docker-save-shaped tar carrying config as its image config blob, and
// returns the image id THAT BLOB HASHES TO. The id is never chosen: a caller that mutates the config
// gets a different identity, which is exactly what the tamper arm needs.
func writeFixtureImage(t *testing.T, path string, config []byte) string {
	t.Helper()
	imageID := "sha256:" + sha256Bytes(config)
	blob := "blobs/sha256/" + strings.TrimPrefix(imageID, "sha256:")
	manifest := []byte(fmt.Sprintf(`[{"Config":%q,"RepoTags":["palai/fixture-engine:t4"],"Layers":[]}]`, blob))
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
	return imageID
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

// --- the unified verifier: clean ------------------------------------------------------------------

// releaseVerify runs the GIT-TRACKED unified verifier, which is the realistic operator workflow and the
// one that satisfies fail-closed resolution structurally: its siblings (provenance-verify.sh,
// sbom-tool.py, runner-verify.sh) are the version-controlled ones, never the release's own copies.
func releaseVerify(t *testing.T, dir, pub string, env ...string) (bool, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", filepath.Join(repoRoot(t), "scripts/release/release-verify.sh"), dir, pub)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// attestedRelease is a complete, signed, internally consistent release plus the out-of-band key that
// verifies it — the baseline every tamper arm starts from and must move OFF of.
func attestedRelease(t *testing.T) (dir, key, pub string) {
	t.Helper()
	dir = stageFullRelease(t)
	key, pub = mintKey(t)
	attest(t, dir, key)
	return dir, key, pub
}

// TestReleaseVerifyAcceptsACleanRelease — the whole index, offline, green. Every claim asserted here is
// the STRONG form (plan §2, the E15 T6 lesson): the counts come from the index and the sbom/ tree, so a
// verifier that silently verified nothing fails this test rather than printing a reassuring line.
func TestReleaseVerifyAcceptsACleanRelease(t *testing.T) {
	dir, _, pub := attestedRelease(t)
	sdk := buildBundle(t)

	ok, out := releaseVerify(t, dir, pub,
		"PALAI_RELEASE_SDK_DIR="+sdk,
		"PALAI_SDK_PUBKEY="+filepath.Join(sdk, "palai-sdk-signing.pub"))
	if !ok {
		t.Fatalf("release-verify.sh FAILED on a clean release:\n%s", out)
	}

	artifacts := indexArtifacts(t, dir)
	entries, err := os.ReadDir(filepath.Join(dir, "sbom"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"release-verify: OK",
		fmt.Sprintf("(3) %d artifact digest(s) recomputed from bytes", len(artifacts)),
		"1 image identity(ies) recomputed from their config blobs",
		fmt.Sprintf("%d declared byproduct(s) recomputed", len(entries)),
		`scan result 'pass'`,
		"(4) SDK bundle",
		// The honest ceiling ships WITH the pass, or the pass overclaims.
		"No identity service was consulted, no transparency log entry exists, no SLSA level is established",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("clean verify output is missing %q:\n%s", want, out)
		}
	}
	// ...and it says so when the SDK family was NOT verified, instead of implying it was.
	if _, out := releaseVerify(t, dir, pub); !strings.Contains(out, "UNVERIFIED by this run") {
		t.Errorf("a run with no SDK bundle must SAY the SDK packages are unverified:\n%s", out)
	}
}

// --- SEC-101: the six-arm tamper matrix -----------------------------------------------------------

// resignWith is `resign` under a NAMED key, spelled out so an arm reads as the attacker model it is.
// The five artifact/metadata arms below re-sign with the REAL key: an outsider with no key is already
// pinned (provenance_test.go's TestProvenanceRejectsTamperedArtifactBytes, sdk_package_test.go's
// TestVerifyFailsOnTamperedPackage) and would make all six arms die at the same fence, proving one
// thing six times. Re-signing models the harder case — a compromised builder, or a release process
// that regenerated its own chain over swapped bytes — and forces each arm onto the leg that
// RECOMPUTES from the canonical source. That is the leg SEC-101 exists to prove.
func resignWith(t *testing.T, dir, key string) { t.Helper(); resign(t, dir, key) }

// editJSON reads, mutates and rewrites a JSON document in the release dir.
func editJSON(t *testing.T, path string, mutate func(map[string]any)) {
	t.Helper()
	var doc map[string]any
	readJSON(t, path, &doc)
	mutate(doc)
	writeJSON(t, path, doc)
}

// TestReleaseVerifyTamperMatrix is SEC-101's promotion half: ONE byte changed in the image tar, an SDK
// package, an SBOM, the provenance, the signature or the index and release-verify.sh DENIES — each arm
// for its OWN reason, named below. The `want` string is the leg that caught it; if a fix ever moves an
// arm onto a different (weaker, earlier) leg, this test says so instead of staying green.
func TestReleaseVerifyTamperMatrix(t *testing.T) {
	for _, tc := range []struct {
		arm string
		// tamper mutates the staged release (and/or the SDK bundle) and re-signs as the arm's threat
		// model requires. key is the release's real signing key.
		tamper func(t *testing.T, dir, sdk, key string)
		want   string
	}{{
		arm: "image tar — one byte in the config blob, index digest honestly re-hashed",
		// The identity leg is the only thing left standing once the file digest has been kept current:
		// a swapped image whose bookkeeping was updated but whose IDENTITY was never re-derived.
		tamper: func(t *testing.T, dir, _, key string) {
			path := filepath.Join(dir, fixtureImage)
			writeFixtureImage(t, path, []byte(`{"architecture":"arm64","os":"linuz","config":{},`+
				`"rootfs":{"type":"layers","diff_ids":[]}}`))
			editJSON(t, filepath.Join(dir, "release-index.json"), func(doc map[string]any) {
				for _, raw := range doc["artifacts"].([]any) {
					if art := raw.(map[string]any); art["file"] == fixtureImage {
						art["digest"] = "sha256:" + sha256File(t, path)
					}
				}
			})
			reattest(t, dir, key)
		},
		want: "the tar does not carry the image the release names",
	}, {
		arm: "SDK package — one byte in the go source tarball",
		tamper: func(t *testing.T, _, sdk, _ string) {
			flipByte(t, filepath.Join(sdk, goPackage))
		},
		want: goPackage,
	}, {
		arm: "SBOM — one byte in an SPDX document",
		tamper: func(t *testing.T, dir, _, key string) {
			flipByte(t, filepath.Join(dir, "sbom", firstSBOM(t, dir)))
			resignWith(t, dir, key)
		},
		want: "the SBOM bytes changed",
	}, {
		arm: "provenance — one byte in the vulnerability decision's byproduct NAME",
		// A single character makes the §51.3 decision unfindable: it is still signed, still present,
		// still hashed — and no longer DECLARED as what it is. That is the 75e5247 finding, mechanised.
		tamper: func(t *testing.T, dir, _, key string) {
			editJSON(t, filepath.Join(dir, statementFile), func(doc map[string]any) {
				run := doc["predicate"].(map[string]any)["runDetails"].(map[string]any)
				for _, raw := range run["byproducts"].([]any) {
					if b := raw.(map[string]any); b["name"] == "vuln-decision" {
						b["name"] = "vuln-decisiom"
					}
				}
			})
			resignWith(t, dir, key)
		},
		want: "declares no `vuln-decision` byproduct",
	}, {
		arm: "signature — the whole release re-signed with the attacker's own P-256 key",
		// An internally PERFECT release: every digest agrees, the chain is whole, the signature verifies
		// — against the wrong key. Only the out-of-band trust root can tell the difference.
		tamper: func(t *testing.T, dir, _, _ string) {
			attacker, _ := mintKey(t)
			resignWith(t, dir, attacker)
		},
		want: "signature check failed against",
	}, {
		arm: "index — one hex character in an artifact's digest",
		tamper: func(t *testing.T, dir, _, key string) {
			editJSON(t, filepath.Join(dir, "release-index.json"), func(doc map[string]any) {
				art := doc["artifacts"].([]any)[0].(map[string]any)
				art["digest"] = flipHex(art["digest"].(string))
			})
			resignWith(t, dir, key)
		},
		want: "is not the file's bytes",
	}} {
		t.Run(tc.arm, func(t *testing.T) {
			dir, key, pub := attestedRelease(t)
			sdk := buildBundle(t)
			env := []string{
				"PALAI_RELEASE_SDK_DIR=" + sdk,
				"PALAI_SDK_PUBKEY=" + filepath.Join(sdk, "palai-sdk-signing.pub"),
			}
			if ok, out := releaseVerify(t, dir, pub, env...); !ok {
				t.Fatalf("baseline verify failed before the tamper:\n%s", out)
			}

			tc.tamper(t, dir, sdk, key)

			ok, out := releaseVerify(t, dir, pub, env...)
			if ok {
				t.Fatalf("release-verify.sh PASSED a tampered release — promotion would be blessed:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the arm failed, but not for its own reason: want output containing %q\n%s", tc.want, out)
			}
			// The promotion half: the promote wrapper must refuse on exactly this.
			if refused, pout := promoteWith(t, dir, pub, env...); !refused {
				t.Errorf("promote.sh did NOT refuse a release that fails release-verify:\n%s", pout)
			}
		})
	}
}

// reattest regenerates the attestation over the dir's CURRENT bytes and re-signs it — the compromised
// builder, who has the key and runs the real tooling over swapped inputs.
func reattest(t *testing.T, dir, key string) {
	t.Helper()
	for _, f := range []string{statementFile, "sha256sums", "sha256sums.sig", "sha256sums.sha256"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	attest(t, dir, key)
}

// firstSBOM names an SPDX document under sbom/ — any one of them; the leg is per-artifact.
func firstSBOM(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "sbom"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".spdx.json") {
			return e.Name()
		}
	}
	t.Fatal("the staged release has no SPDX document to tamper")
	return ""
}

// flipHex changes exactly one hex character of a sha256:… digest string.
func flipHex(digest string) string {
	b := []byte(digest)
	i := len(b) - 1
	if b[i] == '0' {
		b[i] = '1'
	} else {
		b[i] = '0'
	}
	return string(b)
}

// promoteWith runs the REAL promote wrapper against a release dir, under the SAME env the operator
// verified with (an SDK bundle nobody names is a family nobody checks — release-verify says so out loud
// and the promote inherits that honesty). The release name is deliberately one that does not exist: if
// release-verify refuses first (it must), the evidence gate is never reached, so this asserts the
// supply-chain gate runs BEFORE — and independently of — the E15 T6 evidence contract.
func promoteWith(t *testing.T, dir, pub string, env ...string) (bool, string) {
	t.Helper()
	cmd := exec.Command("/usr/bin/env", "bash",
		filepath.Join(repoRoot(t), "scripts/release/promote.sh"), "no-such-release", "rc")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(append(os.Environ(), env...),
		"RELEASE=", "PALAI_RELEASE_DIR="+dir, "PALAI_RELEASE_PUBKEY="+pub)
	out, err := cmd.CombinedOutput()
	return err != nil && strings.Contains(string(out), "did not verify"), string(out)
}

// TestPromoteRefusesAnUnnamedReleaseBeyondRC — the SH-3 half of the promotion gate: a stable flip may not
// bless artifacts nobody verified, so the artifact dir is REQUIRED past rc rather than merely honoured.
func TestPromoteRefusesAnUnnamedReleaseBeyondRC(t *testing.T) {
	cmd := exec.Command("/usr/bin/env", "bash",
		filepath.Join(repoRoot(t), "scripts/release/promote.sh"), "no-such-release", "stable")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "RELEASE=", "PALAI_RELEASE_DIR=", "PALAI_RELEASE_PUBKEY=")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "must name the release artifact directory") {
		t.Fatalf("promote.sh did not refuse a stable promote with no release dir (err=%v):\n%s", err, out)
	}
}

// --- SEC-101, the execution half ------------------------------------------------------------------

// TestTamperedImageIsUnreachableUnderItsPinnedDigest is SEC-101's EXECUTION half, against the digest-pin
// seam that already exists (packages/runner.ParseLeaseOffer, the OCI driver's pin rule).
//
// The promotion half above stops a tampered release at the gate. This one asks the question that
// matters when the gate is bypassed entirely — an operator hand-loads a swapped image tar: can the
// tampered bytes ever RUN under the identity the release pinned? No, and for a structural reason:
//  1. an image's identity IS the sha256 of its config blob, so one changed byte is a different image
//     id — the pinned id no longer names anything the tampered tar contains;
//  2. a run is dispatched by that id and NOTHING ELSE: the runner REFUSES a lease.offer whose image is
//     named by anything but an immutable sha256 digest (ErrMutableLeaseImage), which is the only way
//     an attacker could reach bytes that have no pinned identity — via a tag they can repoint.
//
// CEILING: what the container runtime does when a pinned digest resolves to nothing is the runtime's
// (docker refuses to run an unknown id); this pins the repo's own admission rule, which is the half
// that would otherwise silently accept "run whatever this tag points at today".
func TestTamperedImageIsUnreachableUnderItsPinnedDigest(t *testing.T) {
	dir := stageFullRelease(t)
	var pinned, arch string
	for _, art := range indexArtifacts(t, dir) {
		if art["file"] == fixtureImage {
			pinned, _ = art["image_id"].(string)
			arch, _ = art["ref"].(string)
		}
	}
	if pinned == "" {
		t.Fatal("the staged index pins no image_id — the execution half would be vacuous")
	}

	// One byte in the config blob, and the identity moves.
	tampered := writeFixtureImage(t, filepath.Join(dir, fixtureImage),
		[]byte(`{"architecture":"arm64","os":"linuz","config":{},`+
			`"rootfs":{"type":"layers","diff_ids":[]}}`))
	if tampered == pinned {
		t.Fatal("the tampered image kept its identity — the image id is not derived from the config blob")
	}

	offer := func(image string) contracts.RunnerMessage {
		return contracts.RunnerMessage{
			Protocol: runner.RunnerProtocolV1, Type: "lease.offer",
			LeaseID: "lease_att_sec101", RunID: "run_sec101", AttemptID: "att_sec101", Fence: 1,
			Data: map[string]any{"image_digest": image, "limits": map[string]any{
				"wall_time_ms": 60000, "max_stdout_bytes": 1 << 20, "max_stderr_bytes": 1 << 16,
				"max_frame_bytes": 1 << 20, "max_memory_bytes": 1 << 28, "max_process_count": 64,
			}},
		}
	}

	// The pinned identity is admitted, and the lease carries THAT id — never the tag beside it.
	lease, err := runner.ParseLeaseOffer(offer(pinned))
	if err != nil {
		t.Fatalf("the release's pinned image was not admitted: %v", err)
	}
	if lease.ImageDigest != pinned {
		t.Fatalf("the admitted lease runs %q, not the pinned %q", lease.ImageDigest, pinned)
	}
	// The tampered bytes have their own id, and an operator who hand-loaded them would have to dispatch
	// THAT id — which is not the one the release, the index or the attestation names.
	if lease.ImageDigest == tampered {
		t.Fatal("a lease pinned to the release digest resolved the TAMPERED image")
	}
	// And the only route to bytes with no pinned identity — a mutable reference — is refused outright.
	for _, mutable := range []string{arch, "sha256:" + strings.Repeat("z", 64), "sha256:short", ""} {
		if _, err := runner.ParseLeaseOffer(offer(mutable)); !errors.Is(err, runner.ErrMutableLeaseImage) {
			t.Errorf("ParseLeaseOffer(%q) error = %v, want ErrMutableLeaseImage — a run named by anything"+
				" but an immutable digest can be repointed at tampered bytes", mutable, err)
		}
	}
}

// --- fail-closed resolution, scan coverage, revocations, offline ----------------------------------

// TestReleaseVerifyResolutionIsFailClosed — plan §2, both halves at once: the trust ROOT and the
// verifying CODE must come from outside the thing they verify, or the run refuses. A verifier resolved
// out of the release dir is a verifier a channel attacker replaced with `exit 0`.
func TestReleaseVerifyResolutionIsFailClosed(t *testing.T) {
	dir, _, pub := attestedRelease(t)

	// (a) no key at all.
	cmd := exec.Command("/bin/sh", filepath.Join(repoRoot(t), "scripts/release/release-verify.sh"), dir)
	cmd.Env = append(os.Environ(), "PALAI_RELEASE_PUBKEY=")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("release-verify.sh PASSED with no out-of-band key:\n%s", out)
	} else if !strings.Contains(string(out), "OUT OF BAND") {
		t.Errorf("the refusal does not say where the key must come from:\n%s", out)
	}

	// (b) the verifying code taken FROM the release dir. A copy of this script beside the artifacts —
	// the exact shape sdk-verify.sh's SF-1 fix closed — refuses, and says so, unless the same-session
	// opt-in is set. Copy the script alone: with no siblings it is both "incomplete" and "in-bundle".
	inBundle := filepath.Join(dir, "release-verify.sh")
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts/release/release-verify.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inBundle, b, 0o755); err != nil {
		t.Fatal(err)
	}
	swapped := exec.Command("/bin/sh", inBundle, dir, pub)
	swapped.Env = os.Environ()
	out, err := swapped.CombinedOutput()
	if err == nil {
		t.Fatalf("the release dir's OWN release-verify.sh ran without an opt-in:\n%s", out)
	}
	if !strings.Contains(string(out), "REFUSING") {
		t.Errorf("the in-bundle verifier did not refuse for the right reason:\n%s", out)
	}
}

// TestReleaseVerifyRefusesAnUnscannedRelease — a release must be SCANNED. sbom-tool.py deliberately
// accepts an unscanned dir (it says so rather than claiming a clean result); turning that honesty into
// a REFUSAL at release level is this verifier's own rule, and without it "no findings" reads as "clean".
func TestReleaseVerifyRefusesAnUnscannedRelease(t *testing.T) {
	dir, key, pub := attestedRelease(t)
	// The CONSISTENT unscanned shape sbom.sh --no-scan produces (scanned:false + result:"not-scanned"),
	// which sbom-tool.py verify accepts by design. An inconsistent one would die at leg (2) instead and
	// this test would prove that leg twice rather than release-verify's own rule.
	editJSON(t, filepath.Join(dir, "release-index.json"), func(doc map[string]any) {
		scan := doc["sbom"].(map[string]any)["vulnerability_scan"].(map[string]any)
		scan["scanned"] = false
		scan["result"] = "not-scanned"
		scan["reason"] = "--no-scan"
	})
	reattest(t, dir, key)

	ok, out := releaseVerify(t, dir, pub)
	if ok {
		t.Fatalf("release-verify.sh PASSED an UNSCANNED release:\n%s", out)
	}
	if !strings.Contains(out, "REFUSING an UNSCANNED release") {
		t.Errorf("the refusal does not name the missing scan:\n%s", out)
	}
}

// TestReleaseVerifyMatchesAnOutOfBandRevocationList — the revocation leg is a MATCH against a list the
// operator supplies out of band, never one reached through the release (whoever can swap the artifacts
// can swap a pointer). A list naming this release's own artifact digest must deny it.
func TestReleaseVerifyMatchesAnOutOfBandRevocationList(t *testing.T) {
	dir, _, pub := attestedRelease(t)
	revoked := indexArtifacts(t, dir)[0]["digest"].(string)

	list := filepath.Join(t.TempDir(), "revocations.json")
	writeJSON(t, list, map[string]any{
		"schema":  "palai.release-revocations/v1",
		"revoked": []any{map[string]any{"id": revoked, "reason": "T4 test: known-bad artifact"}},
	})
	ok, out := releaseVerify(t, dir, pub, "PALAI_RELEASE_REVOCATIONS="+list)
	if ok {
		t.Fatalf("release-verify.sh PASSED a REVOKED artifact:\n%s", out)
	}
	if !strings.Contains(out, "REVOKED: "+revoked) {
		t.Errorf("the refusal does not name the revoked id:\n%s", out)
	}

	// A list that cannot be read is not an absence of revocations.
	if ok, out := releaseVerify(t, dir, pub, "PALAI_RELEASE_REVOCATIONS="+list+".missing"); ok {
		t.Errorf("an unreadable revocation list was treated as 'nothing revoked':\n%s", out)
	}
	// ...and a run with NO list says so, instead of implying the release was checked against one.
	if _, out := releaseVerify(t, dir, pub); !strings.Contains(out, "NO revocation list supplied") {
		t.Errorf("a run without a revocation list did not say so:\n%s", out)
	}
}

// TestReleaseVerifyOfflineNetworkNone proves the WHOLE unified verify needs no network by running it in
// a container with no network device. Operator-gated on an already-loaded openssl+python3 image; a green
// package WITHOUT it does not prove the offline claim, which is why the skip says so.
func TestReleaseVerifyOfflineNetworkNone(t *testing.T) {
	image := os.Getenv("PALAI_RELEASE_TOOL_IMAGE")
	if image == "" {
		t.Skip("UNPROVEN IN THIS RUN: the --network none (offline) claim was NOT exercised. Set" +
			" PALAI_RELEASE_TOOL_IMAGE to an already-loaded openssl+python3 image (e.g." +
			" palai/reference-engine:local) to run it")
	}
	dir, _, pub := attestedRelease(t)
	cmd := exec.Command("/bin/sh", filepath.Join(repoRoot(t), "scripts/release/release-verify.sh"),
		"--network-none", dir, pub, image)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("offline (--network none) unified verify failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"release-verify: OK",
		"artifact digest(s) recomputed from bytes",
		"declared byproduct(s) recomputed",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the offline run is missing %q — it did not do the work:\n%s", want, out)
		}
	}
}
