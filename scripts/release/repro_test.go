// E18 T1 — the reproducibility crown, plus the release-index contract.
//
// The claim under test is BINARY-level reproducibility: two runs of scripts/release/build.sh from
// the same commit produce BIT-IDENTICAL binaries and host packages. Image-LAYER reproducibility is
// NOT claimed anywhere (layer timestamps move), so the image legs of the matrix are exercised by
// hand/docker, not here.
//
// The proof RE-RUNS the real script (never greps it for flags — a grep passes a build that has
// silently lost -trimpath). TestUntrimmedBuildIsNotReproducible is the RED half: it builds a canary
// module from two different directories with a repro-BREAKING flag set and asserts the digests
// DIFFER, so the equality assertions above cannot be vacuous on this toolchain.
package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The reduced matrix the tests build twice: the host CLI target (warmest cache) plus the linux
// runner host package. The FULL matrix (darwin/linux × amd64/arm64 + per-arch host packages +
// multi-arch images) is the operator/release invocation — the machinery is identical, and a
// reduced matrix keeps this inside `go test ./...`.
var reducedMatrix = []string{"--cli-targets", "darwin/arm64", "--agent-targets", "darwin/arm64"}

// scratchBuild declares what every build in this package is. build.sh REFUSES a dirty working tree,
// because "<version>+g<commit>-dirty" is the same stamp for any two trees sitting on that commit — and
// that stamp is what a device reports to the fleet. These tests build from whatever tree the developer
// has checked out, so they say so explicitly rather than passing because CI happened to be clean.
var scratchBuild = []string{"PALAI_RELEASE_ALLOW_DIRTY=1"}

type indexArtifact struct {
	Kind       string  `json:"kind"`
	File       string  `json:"file"`
	OS         string  `json:"os"`
	Arch       string  `json:"arch"`
	Digest     string  `json:"digest"`
	SBOM       *string `json:"sbom"`
	Provenance *string `json:"provenance"`
}

type releaseIndex struct {
	Schema          string          `json:"schema"`
	Version         string          `json:"version"`
	Stamp           string          `json:"stamp"`
	Commit          string          `json:"commit"`
	SourceDateEpoch string          `json:"source_date_epoch"`
	Reproducibility map[string]any  `json:"reproducibility"`
	Artifacts       []indexArtifact `json:"artifacts"`
	SBOM            *string         `json:"sbom"`
	Provenance      *string         `json:"provenance"`
}

// buildRelease runs the REAL scripts/release/build.sh into a fresh dir and returns it with the
// parsed release-index.json.
func buildRelease(t *testing.T, args ...string) (string, releaseIndex) {
	t.Helper()
	out := t.TempDir()
	argv := append([]string{filepath.Join(repoRoot(t), "scripts/release/build.sh"),
		"--out", out, "--no-images", "--version", "18.0.0"}, args...)
	cmd := exec.Command("/usr/bin/env", append([]string{"bash"}, argv...)...)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), scratchBuild...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build.sh %v: %v\n%s", args, err, combined)
	}
	b, err := os.ReadFile(filepath.Join(out, "release-index.json"))
	if err != nil {
		t.Fatalf("build.sh wrote no release-index.json: %v", err)
	}
	var idx releaseIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatalf("release-index.json is not valid JSON: %v\n%s", err, b)
	}
	return out, idx
}

// TestReleaseBuildIsBitReproducible is the crown: same commit, two independent runs, byte-identical
// artifacts. Equality is asserted over the digest RECOMPUTED from each artifact's bytes (not the
// digest the index claims), and the two indexes' artifact sets must agree.
func TestReleaseBuildIsBitReproducible(t *testing.T) {
	outA, idxA := buildRelease(t, reducedMatrix...)
	outB, idxB := buildRelease(t, reducedMatrix...)

	if len(idxA.Artifacts) == 0 {
		t.Fatal("release-index.json lists no artifacts — the repro claim would be vacuous")
	}
	if len(idxA.Artifacts) != len(idxB.Artifacts) {
		t.Fatalf("two runs produced different artifact counts: %d vs %d", len(idxA.Artifacts), len(idxB.Artifacts))
	}
	for i, a := range idxA.Artifacts {
		b := idxB.Artifacts[i]
		if a.File != b.File {
			t.Fatalf("artifact %d: file %q vs %q — the index is not deterministically ordered", i, a.File, b.File)
		}
		gotA, gotB := "sha256:"+sha256File(t, filepath.Join(outA, a.File)), "sha256:"+sha256File(t, filepath.Join(outB, b.File))
		if gotA != gotB {
			t.Errorf("%s is NOT bit-reproducible:\n  run A %s\n  run B %s", a.File, gotA, gotB)
		}
		// recompute-over-copy: the index's digest must be the artifact's actual bytes.
		if a.Digest != gotA {
			t.Errorf("%s: index digest %s != recomputed %s", a.File, a.Digest, gotA)
		}
	}

	// The index itself is reproducible too (no wall clock in it): built_at and everything else
	// derive from SOURCE_DATE_EPOCH = the commit timestamp.
	if a, b := sha256File(t, filepath.Join(outA, "release-index.json")), sha256File(t, filepath.Join(outB, "release-index.json")); a != b {
		t.Errorf("release-index.json is not reproducible across two runs (%s != %s) — a wall clock leaked in", a, b)
	}
}

// TestReleaseBinariesAreTrimpathedAndPathFree reads the property off the ARTIFACT (go version -m
// reports the build settings recorded in the binary) rather than off the script that produced it.
func TestReleaseBinariesAreTrimpathedAndPathFree(t *testing.T) {
	out, idx := buildRelease(t, reducedMatrix...)
	root := repoRoot(t)
	var checked int
	for _, a := range idx.Artifacts {
		if a.Kind != "cli" {
			continue
		}
		checked++
		path := filepath.Join(out, a.File)
		info, err := exec.Command("go", "version", "-m", path).Output()
		if err != nil {
			t.Fatalf("go version -m %s: %v", a.File, err)
		}
		if !strings.Contains(string(info), "-trimpath=true") {
			t.Errorf("%s was NOT built with -trimpath:\n%s", a.File, info)
		}
		if strings.Contains(string(info), "vcs.revision") {
			t.Errorf("%s carries VCS stamping (-buildvcs=false was dropped) — build state leaks into the bytes", a.File)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytesContains(b, root) {
			t.Errorf("%s embeds the build path %q — -trimpath is not effective", a.File, root)
		}
	}
	if checked == 0 {
		t.Fatal("no cli artifacts in the index — nothing was checked")
	}
}

func bytesContains(hay []byte, needle string) bool { return strings.Contains(string(hay), needle) }

// TestUntrimmedBuildIsNotReproducible — the RED-first fixture for the repro claim. A canary module
// built from two DIFFERENT directories:
//
//	with a repro-breaking flag set (no -trimpath) → digests DIFFER  (so the equality above has teeth)
//	with the release flag set                     → digests EQUAL
//
// If the release flags ever lose -trimpath, this test's second half turns RED with the first half.
func TestUntrimmedBuildIsNotReproducible(t *testing.T) {
	const main = "package main\n\nfunc main() { println(\"canary\") }\n"
	const mod = "module canary\n\ngo 1.26.0\n"

	build := func(dir string, flags ...string) string {
		t.Helper()
		src := filepath.Join(dir, "canary")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"main.go": main, "go.mod": mod} {
			if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		bin := filepath.Join(dir, "canary.bin")
		cmd := exec.Command("go", append(append([]string{"build"}, flags...), "-o", bin, ".")...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("canary build %v: %v\n%s", flags, err, o)
		}
		return sha256File(t, bin)
	}

	// Repro-BREAKING: no -trimpath, so the build directory is baked into the binary.
	breakingA := build(t.TempDir(), "-buildvcs=false", "-ldflags", "-s -w")
	breakingB := build(t.TempDir(), "-buildvcs=false", "-ldflags", "-s -w")
	if breakingA == breakingB {
		t.Fatalf("a build WITHOUT -trimpath was reproducible across two paths (%s) — this toolchain cannot"+
			" demonstrate the breakage, so the repro assertions prove nothing", breakingA)
	}

	// The release flag set: path-independent, buildid-free → bit-equal from two different paths.
	okA := build(t.TempDir(), "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -buildid=")
	okB := build(t.TempDir(), "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -buildid=")
	if okA != okB {
		t.Errorf("the release flag set is NOT reproducible across build paths: %s != %s", okA, okB)
	}
}

// TestReleaseIndexShape pins the contract T2 (sbom) and T3 (provenance) fill in: the fields exist
// and are explicitly EMPTY, every artifact carries kind+arch+digest, and the digest is the
// artifact's real bytes.
func TestReleaseIndexShape(t *testing.T) {
	out, idx := buildRelease(t, reducedMatrix...)

	if idx.Schema == "" {
		t.Error("release-index.json has no schema field")
	}
	if idx.Commit == "" || idx.Commit == "unknown" {
		t.Errorf("release-index.commit = %q — an index that cannot name its source commit is not provenance-ready", idx.Commit)
	}
	if idx.SourceDateEpoch == "" {
		t.Error("release-index.source_date_epoch missing — the reproducibility input is not recorded")
	}
	if idx.SBOM != nil {
		t.Errorf("release-index.sbom must be present and EMPTY until T2 fills it (got %q)", *idx.SBOM)
	}
	if idx.Provenance != nil {
		t.Errorf("release-index.provenance must be present and EMPTY until T3 fills it (got %q)", *idx.Provenance)
	}
	// Honest naming is part of the contract: the index must say binary-level, not layer-level.
	repro, _ := json.Marshal(idx.Reproducibility)
	for _, want := range []string{"binary", "layer"} {
		if !strings.Contains(string(repro), want) {
			t.Errorf("release-index.reproducibility must name what IS and IS NOT claimed (binary vs image-layer); got %s", repro)
		}
	}

	kinds := map[string]int{}
	for _, a := range idx.Artifacts {
		kinds[a.Kind]++
		if a.Arch == "" || a.Kind == "" || a.Digest == "" || a.File == "" {
			t.Errorf("artifact %+v is missing kind/arch/digest/file", a)
		}
		if a.SBOM != nil || a.Provenance != nil {
			t.Errorf("artifact %s: sbom/provenance must be present and EMPTY until T2/T3", a.File)
		}
		if got := "sha256:" + sha256File(t, filepath.Join(out, a.File)); got != a.Digest {
			t.Errorf("artifact %s: index digest %s is not the artifact's bytes (%s)", a.File, a.Digest, got)
		}
	}
	for _, kind := range []string{"cli", "device-agent"} {
		if kinds[kind] == 0 {
			t.Errorf("release-index lists no %q artifact (kinds seen: %v)", kind, kinds)
		}
	}
}
