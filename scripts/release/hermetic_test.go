// E18 T1 — the PIN guard. Design invariant §2: "digest everywhere, mutable tag nowhere". An
// artifact — including a base image — is identified ONLY by its sha256 digest, so a tag move can
// never change what a pinned build resolves. `engines/reference/Dockerfile` is the in-tree
// precedent; this guard makes it mechanical for every tracked Dockerfile.
//
// Two checks, both fixture-driven so they stay RED-able forever (a guard that only ever sees a
// clean tree cannot prove it has teeth):
//
//	basePinViolations   — every FROM is `name:tag@sha256:<64hex>` (or `scratch`).
//	toolchainDrift      — the golang base tag EQUALS go.mod's `toolchain` directive, so the
//	                      release toolchain version is pinned in ONE place and cannot drift.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pinnedFrom matches an immutable base reference: a human-readable tag AND the digest that the
// build actually resolves. Digest-only (`img@sha256:…`) is rejected on purpose — the tag is what
// tells a reviewer which version the digest is supposed to be, and the reference-engine precedent
// carries both.
var pinnedFrom = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)

// basePinViolations reports every FROM line in one Dockerfile that is not pinned to an immutable
// base. `FROM scratch` is the one legal exception (no image to pin). Stage references
// (`FROM build AS x` / `COPY --from=build`) are internal to the file, so a bare stage name that
// this file declares earlier is not a base at all.
func basePinViolations(content string) []string {
	var out []string
	stages := map[string]bool{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		// Drop `--platform=…`-class flags; keep the reference and any `AS <stage>`.
		var ref string
		for i := 0; i < len(fields); i++ {
			if strings.HasPrefix(fields[i], "--") {
				continue
			}
			ref = fields[i]
			if len(fields) > i+2 && strings.EqualFold(fields[i+1], "AS") {
				stages[fields[i+2]] = true
			}
			break
		}
		switch {
		case ref == "scratch", stages[ref]:
		case pinnedFrom.MatchString(ref):
		default:
			out = append(out, line)
		}
	}
	return out
}

// goToolchain returns the version in go.mod's `toolchain go<ver>` directive ("1.26.4").
func goToolchain(gomod string) string {
	for _, raw := range strings.Split(gomod, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "toolchain go"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

var golangBase = regexp.MustCompile(`(?m)^FROM\s+(?:--\S+\s+)*golang:([A-Za-z0-9._-]+)@sha256:`)

// toolchainDrift reports golang base tags in one Dockerfile that disagree with go.mod's pinned
// toolchain. The digest already makes the base immutable; this makes the VERSION single-sourced, so
// a `go get toolchain` bump cannot silently leave the release image on an older compiler.
func toolchainDrift(content, want string) []string {
	var out []string
	for _, m := range golangBase.FindAllStringSubmatch(content, -1) {
		if m[1] != want {
			out = append(out, "golang:"+m[1]+" != go.mod toolchain go"+want)
		}
	}
	return out
}

// trackedDockerfiles reads every git-TRACKED Dockerfile (untracked scratch files in a dirty tree
// are not release inputs).
func trackedDockerfiles(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "*Dockerfile*").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := map[string]string{}
	for _, rel := range strings.Fields(string(out)) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		files[rel] = string(b)
	}
	if len(files) == 0 {
		t.Fatal("no tracked Dockerfiles found — the guard would be vacuous")
	}
	return files
}

func TestEveryDockerfileBaseIsDigestPinned(t *testing.T) {
	for rel, content := range trackedDockerfiles(t) {
		if v := basePinViolations(content); len(v) > 0 {
			t.Errorf("%s: unpinned base image (§2 digest-everywhere) — pin with name:tag@sha256:<digest>:\n\t%s",
				rel, strings.Join(v, "\n\t"))
		}
	}
}

// TestPinGuardRejectsUnpinnedFixtures is the RED half: the exact shapes a careless edit produces
// must be REJECTED, and the legal shapes accepted. Without this the guard above could be a no-op.
func TestPinGuardRejectsUnpinnedFixtures(t *testing.T) {
	bad := map[string]string{
		"tag only":            "FROM golang:1.26.4 AS build\n",
		"latest":              "FROM alpine:latest\n",
		"no tag at all":       "FROM alpine\n",
		"digest without tag":  "FROM alpine@sha256:" + strings.Repeat("a", 64) + "\n",
		"truncated digest":    "FROM alpine:3.21@sha256:deadbeef\n",
		"platform + tag only": "FROM --platform=$BUILDPLATFORM golang:1.26.4 AS build\n",
		"lowercase from":      "from ubuntu:24.04\n",
	}
	for name, content := range bad {
		if v := basePinViolations(content); len(v) == 0 {
			t.Errorf("fixture %q (%q) was ACCEPTED — the pin guard has no teeth", name, strings.TrimSpace(content))
		}
	}

	good := map[string]string{
		"pinned":            "FROM alpine:3.21@sha256:" + strings.Repeat("b", 64) + "\n",
		"pinned + platform": "FROM --platform=$BUILDPLATFORM golang:1.26.4@sha256:" + strings.Repeat("c", 64) + " AS build\n",
		"scratch":           "FROM scratch\n",
		"stage reference": "FROM golang:1.26.4@sha256:" + strings.Repeat("d", 64) + " AS build\n" +
			"FROM alpine:3.21@sha256:" + strings.Repeat("e", 64) + "\nCOPY --from=build /x /x\n",
	}
	for name, content := range good {
		if v := basePinViolations(content); len(v) > 0 {
			t.Errorf("fixture %q was REJECTED (%v) — the guard is over-strict", name, v)
		}
	}
}

func TestGolangBaseMatchesGoModToolchain(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	want := goToolchain(string(b))
	if want == "" {
		t.Fatal("go.mod has no `toolchain go<ver>` directive — the release toolchain is unpinned")
	}
	var seen int
	for rel, content := range trackedDockerfiles(t) {
		seen += len(golangBase.FindAllString(content, -1))
		if d := toolchainDrift(content, want); len(d) > 0 {
			t.Errorf("%s: toolchain drift: %s", rel, strings.Join(d, "; "))
		}
	}
	if seen == 0 {
		t.Fatal("no pinned golang base found in any Dockerfile — the toolchain check would be vacuous")
	}
}

// TestHostCompileLegsRefuseTheProxy is the hermeticity guard for the legs no Dockerfile covers: the
// CLI matrix and the runner host package compile on the HOST, and release-index.json states as a
// release property that "the compile runs GOPROXY=off against that warmed cache". That string is only
// honest if the host legs are pinned too.
//
// Proven the one way that cannot rot (the repro_test doctrine — never grep the script, a grep passes a
// build that silently lost a flag): RUN each host leg with a COLD GOMODCACHE and an ambient GOPROXY
// that WOULD work, and require it to FAIL with "module lookup disabled by GOPROXY=off". An unpinned leg
// turns this RED either way — it downloads and succeeds, or (behind a firewall) it fails with a network
// error instead. "go: downloading …" is deliberately NOT the discriminator: the go command logs that
// line before it consults the proxy list, so it appears in the pinned case too.
//
// BOTH legs are invoked separately on purpose: build.sh aborts at the first leg that fails, so running
// it alone would leave the runner packager's own pinning unproven.
func TestHostCompileLegsRefuseTheProxy(t *testing.T) {
	root := repoRoot(t)
	legs := map[string]struct {
		argv []string
		env  []string
	}{
		"cli matrix (scripts/release/build.sh)": {
			argv: []string{filepath.Join(root, "scripts/release/build.sh"),
				"--out", t.TempDir(), "--no-images", "--version", "18.0.0",
				"--cli-targets", "darwin/arm64", "--runner-archs", "arm64"},
		},
		"runner host package (scripts/package/runner/build.sh)": {
			argv: []string{filepath.Join(root, "scripts/package/runner/build.sh")},
			env:  []string{"VERSION=18.0.0", "ARCH=arm64", "OUT=" + t.TempDir()},
		},
	}
	for name, leg := range legs {
		t.Run(name, func(t *testing.T) {
			cold := filepath.Join(t.TempDir(), "cold")
			// A leg that has LOST its pinning downloads into this cache, and module cache dirs are
			// 0555 — so make them writable before t.TempDir's own cleanup runs (LIFO), else a RED run
			// leaves ~100MB of undeletable litter in $TMPDIR.
			t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+w", cold).Run() })
			cmd := exec.Command("/usr/bin/env", append([]string{"bash"}, leg.argv...)...)
			cmd.Dir = root
			// Cold cache + the DEFAULT proxy + no ambient GOFLAGS: the only thing that can stop a
			// download is the script's own pinning, so this cannot pass for the wrong reason.
			cmd.Env = append(append(os.Environ(),
				"GOMODCACHE="+cold,
				"GOPROXY=https://proxy.golang.org,direct",
				"GOFLAGS=",
			), leg.env...)
			o, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("SUCCEEDED with a COLD module cache — this leg resolved through the module proxy,"+
					" so the release index's `hermetic` string is FALSE for its artifacts:\n%s", o)
			}
			if !strings.Contains(string(o), "module lookup disabled by GOPROXY=off") {
				t.Errorf("failed on a cold cache, but not because the module lookup was DISABLED — any"+
					" other failure proves nothing about GOPROXY=off:\n%s", o)
			}
		})
	}
}

// TestToolchainGuardRejectsDriftFixture — the RED half of the toolchain check.
func TestToolchainGuardRejectsDriftFixture(t *testing.T) {
	const drifted = "FROM golang:1.25.0@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000" + " AS build\n"
	if d := toolchainDrift(drifted, "1.26.4"); len(d) == 0 {
		t.Error("a golang base one minor behind go.mod was ACCEPTED — the toolchain guard has no teeth")
	}
	if got := goToolchain("module x\n\ngo 1.26.0\n\ntoolchain go1.26.4\n"); got != "1.26.4" {
		t.Errorf("goToolchain = %q, want 1.26.4", got)
	}
	if got := goToolchain("module x\n\ngo 1.26.0\n"); got != "" {
		t.Errorf("goToolchain on a toolchain-less go.mod = %q, want empty", got)
	}
}
