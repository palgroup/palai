package toolbroker_test

import (
	"reflect"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestTheSynchronousShellSeamIsFieldForFieldUnchanged is E26's third RED, in the only form that keeps
// meaning after the day it was written. The claim is that a shell call which does not use `background`
// behaves exactly as it did — and the seam it behaves through is these two structs.
//
// ShellCommand IS REUSED, NOT EXTENDED, and that is the sharper half. E24 measured this type's
// serialisability when it planned to send one to another machine, and the relay that will need that
// property is still ahead (E27). A field added here for a background task would be a field meaningless
// on the far side of a wire — so detachment lives in a SECOND argument (BackgroundSpec) and this type
// stays what it was.
//
// ShellResult is the model-facing half: a field appearing or changing type here changes what every
// non-background tool call returns, which is the definition of the regression this task must not cause.
//
// ‼️ ShellCommand's BASELINE MOVED ONCE, 2026-08-05, A.5 T3 — five fields to six, adding RunAs. It is
// recorded here rather than silently re-counted, because a guard whose number changes without a reason
// beside it stops being a guard. Two things make it a legitimate move rather than the regression this
// test exists to catch:
//
//   - IT IS NOT A BACKGROUND FIELD. The sentence in the error below — "a background feature must not
//     change what a non-background call sends" — still holds: RunAs is read identically by the
//     synchronous and the detached path (adapters/sandboxes/host's procAttrFor is called by both), so a
//     `background: true` call and a plain one differ by exactly what they always differed by.
//   - THE OBJECTION THE PARAGRAPH ABOVE RAISES IS ANSWERED RATHER THAN OVERRULED. It says a field added
//     here would be "meaningless on the far side of a wire, and the relay that will need that property
//     is still ahead (E27)". That relay SHIPPED — packages/runner/toolserver.go marshals this struct
//     onto runner.v1 — and RunAs is the one field that is meaningless on THIS side: the control plane
//     mints the uid and the machine is the only thing that can spend it.
//
// ShellResult has never moved and this test would still be worth having if that were all it asserted.
func TestTheSynchronousShellSeamIsFieldForFieldUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  any
		fields map[string]string
	}{
		{"ShellCommand", toolbroker.ShellCommand{}, map[string]string{
			"Argv":          "[]string",
			"WorkspaceRoot": "string",
			"ReadOnly":      "bool",
			"Shell":         "bool",
			"Env":           "map[string]string",
			"RunAs":         "*toolbroker.RunAs",
		}},
		{"ShellResult", toolbroker.ShellResult{}, map[string]string{
			"ExitCode":   "int",
			"Signal":     "string",
			"Stdout":     "string",
			"Stderr":     "string",
			"Truncated":  "bool",
			"TimedOut":   "bool",
			"OOMKilled":  "bool",
			"DurationMS": "int64",
		}},
	} {
		typ := reflect.TypeOf(tc.value)
		if typ.NumField() != len(tc.fields) {
			t.Errorf("%s has %d fields, want the %d this seam is pinned to (see the recorded baseline move above)",
				tc.name, typ.NumField(), len(tc.fields))
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			want, declared := tc.fields[field.Name]
			if !declared {
				t.Errorf("%s gained a field %q: a background feature must not change what a non-background "+
					"call sends or receives", tc.name, field.Name)
				continue
			}
			if got := field.Type.String(); got != want {
				t.Errorf("%s.%s is %s, want %s", tc.name, field.Name, got, want)
			}
		}
	}
}

// TestABackgroundOutputPathCannotLeaveTheAllocation keeps the containment in the one place both postures
// share it. Every path comparison this tree has shipped in two copies has been defeated in one of them,
// so there is one copy.
func TestABackgroundOutputPathCannotLeaveTheAllocation(t *testing.T) {
	root := "/tmp/alloc"
	for _, path := range []string{
		"../escape.log",
		"../../etc/passwd",
		"/etc/passwd",
		".palai-session/bg/../../../escape.log",
		"",
	} {
		spec := toolbroker.BackgroundSpec{TaskID: "bgt-1", OutputPath: path}
		if _, err := spec.Resolve(root); err == nil {
			t.Errorf("output path %q resolved inside the allocation; it does not", path)
		}
	}

	spec := toolbroker.BackgroundSpec{TaskID: "bgt-1", OutputPath: ".palai-session/bg/bgt-1.log"}
	got, err := spec.Resolve(root)
	if err != nil {
		t.Fatalf("a contained path was refused: %v", err)
	}
	if got != "/tmp/alloc/.palai-session/bg/bgt-1.log" {
		t.Fatalf("resolved to %q", got)
	}

	// A spec with no task id names no log file and labels no container; it is refused rather than given a
	// default, because a default would make two tasks share a name.
	if _, err := (toolbroker.BackgroundSpec{OutputPath: "x.log"}).Resolve(root); err == nil {
		t.Error("a spec with no task id was accepted")
	}
	if _, err := spec.Resolve(""); err == nil {
		t.Error("a spec with no workspace root was accepted")
	}
}

// The compile-time conformance assertions live in the two ADAPTERS
// (adapters/sandboxes/host/background.go, adapters/sandboxes/oci/workspace/background.go) rather than
// here, and deliberately: this package stays dependency-light, and a test binary that imported the OCI
// adapter to check an interface would pull the whole Docker client into the broker's own tier.
