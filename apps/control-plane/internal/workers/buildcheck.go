package workers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BuildResult is the structured output of the swift.build-check typed operation. Mode is named HONESTLY:
// "real-swiftc" when a real swiftc type-check ran, "toy" when the host has no Swift toolchain and a
// deterministic syntactic check stood in. NEITHER is a macOS/iOS BUILD: -typecheck parses and type-checks
// only, with no codegen, no linking, and NO signing — the honest ceiling (§6 leg 3). Diagnostics is the
// compiler/checker output; ArtifactSHA256 is the digest of the produced report artifact.
type BuildResult struct {
	Mode           string `json:"mode"`
	OK             bool   `json:"ok"`
	Diagnostics    string `json:"diagnostics"`
	ToolchainPath  string `json:"toolchain_path"`
	ArtifactSHA256 string `json:"artifact_sha256"`
}

// BuildCheck runs the swift.build-check operation on the given Swift source. It RUNTIME-detects the host
// toolchain: if swiftc is on PATH it runs a REAL `swiftc -typecheck` (a genuine compile-check — parse + type-
// check, no build, no signing); otherwise it falls back to a deterministic toy check (balanced braces + a
// top-level declaration) and names the mode "toy" so no result ever overclaims. It is read-only: it produces
// a report artifact but touches no external side effect. The returned bytes are the report artifact.
func BuildCheck(ctx context.Context, source []byte) (BuildResult, []byte) {
	if path, err := exec.LookPath("swiftc"); err == nil {
		return realSwiftCheck(ctx, path, source)
	}
	return toyBuildCheck(source)
}

// realSwiftCheck writes the source to a throwaway file and runs `swiftc -typecheck`. A zero exit is a passing
// check; a non-zero exit carries the diagnostics. -typecheck deliberately does NOT emit a binary — there is
// no build product and no signing step, so this proves only that the host toolchain type-checks the source.
func realSwiftCheck(ctx context.Context, swiftc string, source []byte) (BuildResult, []byte) {
	dir, err := os.MkdirTemp("", "swift-build-check-*")
	if err != nil {
		return toyBuildCheck(source) // no scratch space: fall back honestly rather than fail the op
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "main.swift")
	if err := os.WriteFile(src, source, 0o600); err != nil {
		return toyBuildCheck(source)
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, swiftc, "-typecheck", src)
	out, err := cmd.CombinedOutput()
	res := BuildResult{Mode: "real-swiftc", OK: err == nil, Diagnostics: strings.TrimSpace(string(out)), ToolchainPath: swiftc}
	report := buildReport(res)
	res.ArtifactSHA256 = digest(report)
	return res, report
}

// toyBuildCheck is the deterministic fallback when no Swift toolchain is present: it checks the source has
// balanced braces and at least one top-level declaration. It is NAMED "toy" so the result is never mistaken
// for a real compile.
func toyBuildCheck(source []byte) (BuildResult, []byte) {
	text := string(source)
	balanced := strings.Count(text, "{") == strings.Count(text, "}")
	hasDecl := strings.Contains(text, "func ") || strings.Contains(text, "let ") ||
		strings.Contains(text, "var ") || strings.Contains(text, "struct ") || strings.Contains(text, "class ")
	ok := balanced && hasDecl
	diag := "toy check passed"
	if !balanced {
		diag = "toy check: unbalanced braces"
	} else if !hasDecl {
		diag = "toy check: no top-level declaration"
	}
	res := BuildResult{Mode: "toy", OK: ok, Diagnostics: diag}
	report := buildReport(res)
	res.ArtifactSHA256 = digest(report)
	return res, report
}

func buildReport(res BuildResult) []byte {
	var b strings.Builder
	b.WriteString("swift.build-check report\n")
	b.WriteString("mode: " + res.Mode + "\n")
	if res.OK {
		b.WriteString("result: pass\n")
	} else {
		b.WriteString("result: fail\n")
	}
	b.WriteString("diagnostics:\n")
	b.WriteString(res.Diagnostics)
	b.WriteString("\n")
	return []byte(b.String())
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
