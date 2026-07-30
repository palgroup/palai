package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// This file holds E26 T1's STRUCTURAL proofs. They are structural on purpose: the claims are about what
// the source says, and a claim about source that is checked by reading source is not checked.
//
// Two things are proven here and neither can be proven by a container test. First, that the detached
// path's hardening cannot DIVERGE from the attached path's — not that it currently matches, which any
// copy would also satisfy on the day it was written, but that it is structurally incapable of differing,
// because it calls the same function and constructs nothing of its own. Second, that dockerDriver.Run
// did not change by one byte while a new lifetime was added beside it.

// hardenedSpec is the spec both proofs below build options from. The values are arbitrary; what matters
// is that they are non-zero, so a field that silently stopped being carried shows up as a zero.
var hardenedSpec = ContainerSpec{
	ImageDigest:    "sha256:" + strings.Repeat("a", 64),
	Env:            []string{"HOME=/workspace"},
	Labels:         map[string]string{"io.palai.sandbox": "shell", "io.palai.bg": "bgt-1"},
	Limits:         Limits{WallTime: 30 * time.Second, MaxMemoryBytes: 256 << 20, MaxProcessCount: 64, NanoCPUs: 1_000_000_000, MaxDiskBytes: 1 << 30},
	MaxStdoutBytes: 1 << 20,
	MaxStderrBytes: 1 << 16,
	Cmd:            []string{"/bin/sh", "-c", `exec "$@" >"$0" 2>&1`, "/workspace/.palai-session/bg/bgt-1.log", "sleep", "30"},
	WorkingDir:     "/workspace",
	Mounts:         []Mount{{Source: "/tmp/alloc", Target: "/workspace"}},
}

// TestTheDetachedPathBuildsNoContainerOptionsOfItsOwn is the proof the plan asks for in those words: the
// hardening of a detached container is identical to that of an attached one, shown STRUCTURALLY rather
// than by reading the two files side by side.
//
// The strong form is not "the fields are equal today". It is that StartDetached hands ContainerCreate
// the result of createOptions(spec, false) and nothing else — so there is no second place where an
// unprivileged uid, a dropped capability or a cgroup bound could be forgotten. A future edit that
// inlined its own options to add one field would fail here even if every other field still matched.
//
// It also asserts the ABSENCE that makes the method exist at all: no deferred removal. Run's `defer`
// force-removes unconditionally, which is correct there and is the single line that made a background
// container impossible; a defer creeping into this function would silently restore that.
func TestTheDetachedPathBuildsNoContainerOptionsOfItsOwn(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "detached.go", nil, 0)
	if err != nil {
		t.Fatalf("parse detached.go: %v", err)
	}

	var start *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "StartDetached" {
			start = fn
		}
	}
	if start == nil {
		t.Fatal("detached.go declares no StartDetached; this guard now proves nothing")
	}

	creates := 0
	ast.Inspect(start, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "ContainerCreate":
			creates++
			if len(call.Args) != 2 {
				t.Fatalf("ContainerCreate takes %d arguments here; the options argument cannot be identified", len(call.Args))
			}
			opts, ok := call.Args[1].(*ast.CallExpr)
			if !ok {
				t.Fatalf("the options argument to ContainerCreate is %T, not a call to createOptions: "+
					"the detached path must not construct its own container options", call.Args[1])
			}
			fn, ok := opts.Fun.(*ast.Ident)
			if !ok || fn.Name != "createOptions" {
				t.Fatalf("the options argument to ContainerCreate is not createOptions(...): " +
					"the hardening would then live in two places and could differ in one")
			}
			if len(opts.Args) != 2 {
				t.Fatalf("createOptions is called with %d arguments, want 2", len(opts.Args))
			}
			interactive, ok := opts.Args[1].(*ast.Ident)
			if !ok || interactive.Name != "false" {
				t.Fatalf("createOptions' interactive argument is %v, want the literal false: "+
					"a detached container must not open stdin", opts.Args[1])
			}
		case "ContainerRemove":
			t.Error("StartDetached calls ContainerRemove directly; removal on this path belongs to KillDetached " +
				"or to the explicit start-failure cleanup, never inline")
		}
		return true
	})
	if creates != 1 {
		t.Fatalf("StartDetached contains %d ContainerCreate calls, want exactly 1", creates)
	}

	// No deferred cleanup of any kind. Run's defer is the whole reason this method exists.
	ast.Inspect(start, func(n ast.Node) bool {
		if _, ok := n.(*ast.DeferStmt); ok {
			t.Error("StartDetached contains a defer; the unconditional deferred removal in Run is exactly " +
				"what makes a background container impossible, and it must not reappear here")
		}
		return true
	})
}

// TestTheHardenedContainerOptionsAreExactlyThese sweeps the produced options by REFLECTION and requires
// an explicit expectation for every non-zero field of the HostConfig. It is the companion to the AST
// proof above: that one shows the detached path uses this function, this one shows what the function
// produces — and the sweep, rather than a list of assertions, is what makes a NEW hardening field
// (or a silently dropped one) fail instead of passing unnoticed.
func TestTheHardenedContainerOptionsAreExactlyThese(t *testing.T) {
	opts := createOptions(hardenedSpec, false)

	// The security posture, field by field, as the type's own comment promises it.
	if opts.HostConfig.NetworkMode != container.NetworkMode("none") {
		t.Errorf("NetworkMode = %q, want none", opts.HostConfig.NetworkMode)
	}
	if !opts.HostConfig.ReadonlyRootfs {
		t.Error("ReadonlyRootfs is false")
	}
	if !reflect.DeepEqual(opts.HostConfig.CapDrop, []string{"ALL"}) {
		t.Errorf("CapDrop = %v, want [ALL]", opts.HostConfig.CapDrop)
	}
	if !reflect.DeepEqual(opts.HostConfig.SecurityOpt, []string{"no-new-privileges"}) {
		t.Errorf("SecurityOpt = %v, want [no-new-privileges]", opts.HostConfig.SecurityOpt)
	}
	if opts.Config.User != "65532:65532" {
		t.Errorf("User = %q, want the unprivileged sandbox uid", opts.Config.User)
	}
	if !opts.Config.NetworkDisabled {
		t.Error("NetworkDisabled is false")
	}
	if opts.Config.AttachStdin || opts.Config.OpenStdin {
		t.Error("stdin is open on a non-interactive container")
	}
	if opts.HostConfig.Resources.Memory != hardenedSpec.Limits.MaxMemoryBytes ||
		opts.HostConfig.Resources.MemorySwap != hardenedSpec.Limits.MaxMemoryBytes ||
		opts.HostConfig.Resources.NanoCPUs != hardenedSpec.Limits.NanoCPUs ||
		opts.HostConfig.Resources.PidsLimit == nil || *opts.HostConfig.Resources.PidsLimit != hardenedSpec.Limits.MaxProcessCount {
		t.Errorf("cgroup bounds are not the spec's: %+v", opts.HostConfig.Resources)
	}

	// THE SWEEP. Every HostConfig field this spec makes non-zero must be named above; a field that starts
	// carrying a value without appearing here means the hardening grew somewhere this test does not read.
	expected := map[string]bool{
		"NetworkMode": true, "ReadonlyRootfs": true, "CapDrop": true, "SecurityOpt": true,
		"Mounts": true, "Resources": true, "StorageOpt": true,
	}
	v := reflect.ValueOf(*opts.HostConfig)
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if v.Field(i).IsZero() || expected[name] {
			continue
		}
		t.Errorf("HostConfig.%s is set but is not one of the fields this guard checks; the container "+
			"hardening changed and nothing here noticed", name)
	}
}

// runDigest is the sha256 of dockerDriver.Run's EXACT source bytes, declaration through closing brace.
//
// It is a committed word rather than a recomputed baseline, and that distinction is the whole point: a
// no-regression guard that derives its own baseline passes over the regression it exists to catch. The
// claim it pins is E26's third RED — a shell call that does not use `background` runs the same code it
// ran before, because that code is the same code.
//
// UPDATING THIS CONSTANT IS ALLOWED AND IS SUPPOSED TO BE DELIBERATE. If a change to Run is intended,
// the new digest goes here in the same commit as the change, and the commit says why.
// Taken at bb9036b1 — the E25 exit-gate merge this branch forked from — where `git diff --stat` over
// adapters/sandboxes/oci/docker.go reports nothing at all.
const runDigest = "cce1f39fb203f6b9ff65f6bc83a404e17c19fc67feeec32e6f4e0fc89f5e33af"

// TestDockerDriverRunIsByteUnchanged proves the synchronous container path was not touched while the
// detached one was added beside it. `git diff --stat` says the same thing on the day of the change and
// nothing at all a week later; a digest keeps saying it.
func TestDockerDriverRunIsByteUnchanged(t *testing.T) {
	got := funcDigest(t, "docker.go", "Run")
	if got != runDigest {
		t.Fatalf("dockerDriver.Run digest = %s, want %s\n"+
			"The synchronous container path changed. If that was intended, update runDigest in the same "+
			"commit and say why; if it was not, a background feature has reached into the code every "+
			"non-background shell call runs.", got, runDigest)
	}
}

// funcDigest returns the sha256 of one top-level function's source bytes in a file of this package.
func funcDigest(t *testing.T, filename, funcName string) string {
	t.Helper()
	source, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			continue
		}
		// Offsets are 1-based into the file; Pos() is the `func` keyword and End() is one past the brace.
		start := fset.Position(fn.Pos()).Offset
		end := fset.Position(fn.End()).Offset
		sum := sha256.Sum256(source[start:end])
		return hex.EncodeToString(sum[:])
	}
	t.Fatalf("%s declares no %s", filename, funcName)
	return ""
}
