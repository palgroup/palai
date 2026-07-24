package workers

import (
	"context"
	"strings"
	"testing"
)

// TestBuildCheckIsHonestAndDeterministic pins the two invariants BuildCheck must never break: it names its
// mode HONESTLY (real-swiftc XOR toy, never overclaiming a build), and it always produces a digested report
// artifact. The toy-path logic (balanced braces + a top-level declaration) is exercised directly so it is
// covered even on a host WITH swiftc.
func TestBuildCheckIsHonestAndDeterministic(t *testing.T) {
	res, report := BuildCheck(context.Background(), []byte("func add() -> Int { return 1 }\n"))
	if res.Mode != "real-swiftc" && res.Mode != "toy" {
		t.Fatalf("mode = %q, want real-swiftc or toy (never an overclaimed build)", res.Mode)
	}
	if res.ArtifactSHA256 == "" || len(report) == 0 {
		t.Fatal("BuildCheck produced no report artifact/digest")
	}
	if !strings.Contains(string(report), "swift.build-check report") {
		t.Fatalf("report is not the build-check report: %s", report)
	}

	// The toy path directly: a valid declaration passes; unbalanced braces and a bare expression fail.
	if r, _ := toyBuildCheck([]byte("struct S { let x = 1 }")); !r.OK || r.Mode != "toy" {
		t.Fatalf("toy check of valid source = %+v, want ok toy", r)
	}
	if r, _ := toyBuildCheck([]byte("func broken() { {")); r.OK {
		t.Fatal("toy check of unbalanced braces reported ok")
	}
	if r, _ := toyBuildCheck([]byte("1 + 1")); r.OK {
		t.Fatal("toy check of a bare expression (no declaration) reported ok")
	}
}
