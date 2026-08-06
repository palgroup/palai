package main

// THE DEVICE'S OWN agentd VERB: it installs the daemon the archive carries, and it never builds one.
//
// ‼️ THE OPERATOR CLI'S VERSION RESOLVED palai-agentd WITH `go build ./cmd/palai-agentd` AND REFUSED
// WITHOUT A SOURCE TREE. That is the defect §3.7 already deleted for the runner, standing one binary
// over: a fleet Mac has no checkout and no Go toolchain, so `accounts` isolation — the only isolation
// that is a cross-customer boundary — was unreachable on exactly the machines it exists for.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheDaemonIsResolvedBESIDETheBinaryAndNeverBuilt is the rule, over the real resolver.
func TestTheDaemonIsResolvedBESIDETheBinaryAndNeverBuilt(t *testing.T) {
	// The resolver reads os.Executable(), which under `go test` is the test binary — so the sibling it
	// looks for is beside THAT. Writing one there is what makes this drive the real path.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(exe), "palai-agentd")
	if _, err := os.Stat(sibling); err == nil {
		t.Skip("a palai-agentd already sits beside the test binary; this test owns that path")
	}

	// Absent: the refusal must name the archive, because that is where an operator gets it — not a
	// compiler, not a download.
	_, err = agentdSource()
	if err == nil {
		t.Fatal("agentdSource accepted a machine with no packaged daemon")
	}
	if !strings.Contains(err.Error(), "archive") {
		t.Fatalf("the refusal does not tell an operator where the daemon comes from: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "go build") || strings.Contains(err.Error(), "source tree") {
		t.Fatalf("the device is being told to BUILD the daemon: %v", err)
	}

	// Present: it is resolved from the executable's own directory, never from PATH — the archive puts
	// them together, and a PATH lookup would find whichever palai-agentd the machine happened to have.
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(sibling) })
	got, err := agentdSource()
	if err != nil {
		t.Fatalf("agentdSource refused the packaged daemon beside the binary: %v", err)
	}
	if got != sibling {
		t.Fatalf("agentdSource = %q, want the sibling %q", got, sibling)
	}
}

// TestTheDeviceHasTWOVerbsAndAgentdIsTheSecond keeps the surface honest: an unknown subcommand is named
// rather than silently starting the agent, which is what os.Args dispatch does when nobody checks.
func TestTheDeviceHasTWOVerbsAndAgentdIsTheSecond(t *testing.T) {
	var out strings.Builder
	if err := runAgentd(context.Background(), nil, &out); err == nil {
		t.Fatal("`palai agentd` with no subcommand did something")
	}
	err := runAgentd(context.Background(), []string{"reinstall"}, &out)
	if err == nil {
		t.Fatal("an unknown agentd subcommand was accepted")
	}
	if !strings.Contains(err.Error(), "reinstall") {
		t.Fatalf("the refusal does not name what was typed: %v", err)
	}
}
