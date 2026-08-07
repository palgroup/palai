package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/macagent"
)

// TestSessionAccountsAreOneInstanceAcrossBothHalves guards a defect that was written and caught in the
// same sitting, and it is worth stating plainly because the broken version COMPILED, VETTED and read like
// working code.
//
// The lifecycle has two halves in two scopes: the acquire is wired onto the orchestrator inside
// startDispatch, and the release onto the command surface beside routerOpts in main. Each was gated on
// the same environment variable, so each constructed its own SlotAccounts. They share a map — which
// session holds which slot — so the releaser's copy was empty: every close would have found nothing to
// destroy AND REPORTED SUCCESS, leaking one account per session on a resource bounded at 99 per machine,
// produced by the code whose whole job is isolating sessions.
//
// Nothing about that fails a build. The property is "constructed once", so that is what this asserts, by
// reading the composition root the way this tree reads a CLI's tool list rather than duplicating it.
func TestSessionAccountsAreOneInstanceAcrossBothHalves(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v — if the composition root moved, this guard must follow it rather than be deleted: %v", err, err)
	}
	body := string(src)

	constructions := regexp.MustCompile(`execution\.NewDaemonSessionAccounts\(`).FindAllString(body, -1)
	if len(constructions) != 1 {
		t.Fatalf("execution.NewDaemonSessionAccounts is called %d times; it must be called exactly ONCE. Two "+
			"instances do not share the map of which session holds which slot, so the releasing half would "+
			"find an empty map, destroy nothing, and report success — one leaked account per session",
			len(constructions))
	}

	// AND BOTH HALVES MUST STILL BE REACHED. "Constructed once" is satisfied by constructing it once and
	// wiring it nowhere, which would be a quieter version of the same bug.
	for _, want := range []struct{ call, why string }{
		{"orch.SetSessionAccounts(", "the acquire half: without it a session never gets its own uid"},
		{"api.WithSessionAccounts(", "the release half: without it an account is minted and never destroyed"},
	} {
		if !strings.Contains(body, want.call) {
			t.Errorf("main.go does not call %s — %s", want.call, want.why)
		}
	}

	// The single value has to REACH the other scope, which for startDispatch means being a parameter. A
	// re-read of the environment variable there would be a second construction wearing a different name.
	if !strings.Contains(body, "sessionAccounts *execution.SlotAccounts") {
		t.Error("startDispatch does not take the accounts as a parameter, so the acquire half is reading " +
			"something other than the value the release half holds")
	}
	// The daemon socket is named ONCE, for the reason the env var was read once before it: a second
	// mention is how a second instance gets built without either call site looking wrong. It replaced
	// PALAI_SESSION_ACCOUNT_HELPER on 2026-08-07 — an env var nobody set, pointing at a sudo wrapper the
	// daemon exists to make unnecessary, which meant isolation was off on every Mac that ran this.
	if n := strings.Count(body, "macagent.DefaultSocketPath"); n != 1 {
		t.Errorf("macagent.DefaultSocketPath is named %d times in the composition root; it must be named "+
			"ONCE, or two SlotAccounts can dial the same daemon while holding different slot maps", n)
	}
}

// startStandInDaemon binds a socket that answers `list` the way palai-agentd does, so the probe below
// measures a daemon that is REACHABLE rather than a boolean a test wrote.
func startStandInDaemon(t *testing.T, sock string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("binding the stand-in daemon on %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, macagent.MaxRequestBytes)
				if _, err := conn.Read(buf); err != nil {
					return
				}
				_, _ = conn.Write([]byte("ok list\n"))
			}()
		}
	}()
}

// TestTheAccountLayerIsBUILTONLYWHENTheDaemonAnswers — THE PLANE MUST RUN WHERE THE MECHANISM DOES NOT.
//
// `accounts` is one of three isolation modes this product ships. The layer was constructed
// unconditionally until 2026-08-08, so on every machine without palai-agentd — every Linux host, and
// every Mac where no administrator ran the install — the FIRST thing a session did was dial a socket
// that is not there, fail Acquire, and never provision a workspace at all. A plane that cannot serve a
// session on Linux is not a plane; it is one platform's agent.
//
// Both arms are driven, and the negative one is the one with history: a test that only proved the
// positive would pass on a machine where the daemon happens to be installed and say nothing about the
// deployment that has none.
func TestTheAccountLayerIsBUILTONLYWHENTheDaemonAnswers(t *testing.T) {
	if runtime.GOOS != "darwin" {
		// The non-darwin arm is the function's first line and needs no socket: a Linux plane skips the
		// mechanism entirely, which is the property this test is named for.
		if got := daemonSessionAccounts(context.Background(), filepath.Join(t.TempDir(), "s")); got != nil {
			t.Fatal("a non-macOS control plane built the macOS account layer")
		}
		return
	}

	absent := filepath.Join(t.TempDir(), "palai-agentd.sock")
	if got := daemonSessionAccounts(context.Background(), absent); got != nil {
		t.Error("the account layer was built against a socket nothing is listening on: every session on this " +
			"machine would fail at Acquire before a workspace was ever provisioned")
	}

	// A short path: a unix socket's address is bounded at 104 bytes on Darwin and t.TempDir() is long.
	dir, err := os.MkdirTemp("", "pa")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	present := filepath.Join(dir, "d.sock")
	startStandInDaemon(t, present)
	if got := daemonSessionAccounts(context.Background(), present); got == nil {
		t.Error("a daemon answered on the socket and the account layer was still not built: this machine " +
			"declares `accounts` isolation to the fleet and would run every session as the operator's own uid")
	}
}
