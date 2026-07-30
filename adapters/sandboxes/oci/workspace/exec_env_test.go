package workspace_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file is the CONTAINER half of E25 T3's exec-time key rule, and both halves exist because the
// rule is enforced in two adaptors and a rule enforced twice is a rule that can hold in one place only.
// Its host sibling is TestHostShellRefusesAnEnvironmentKeyThatShadowsItsOwn
// (adapters/sandboxes/host/exec_test.go).
//
// It is UNTAGGED, so it rides `make verify`. That is deliberate: the container executor's own component
// suite needs Docker, and a proof that only runs where Docker runs is a proof that does not run.

// recordingDriver captures the ContainerSpec instead of creating a container. It records EVERY spec it
// is handed, so a test can assert not only what a refusal returned but that nothing was created at all.
type recordingDriver struct {
	specs   []oci.ContainerSpec
	outcome oci.Outcome
}

func (d *recordingDriver) Run(_ context.Context, spec oci.ContainerSpec) (oci.Outcome, error) {
	d.specs = append(d.specs, spec)
	return d.outcome, nil
}

func (d *recordingDriver) Close() error { return nil }

const testImage = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newShellExecutor(driver oci.Driver) *workspace.ShellExecutor {
	return workspace.NewShellExecutor(driver, testImage, oci.Limits{
		WallTime: time.Minute, MaxMemoryBytes: 1 << 28, MaxProcessCount: 64, NanoCPUs: 1e9,
	})
}

// TestContainerShellLayersTheAttemptsEnvironmentOnTopOfItsOwn pins the whole of what the container
// posture does with ShellCommand.Env: shellEnv()'s two entries arrive UNCHANGED and the attempt's keys
// arrive beside them. Asserted as a set rather than as a joined string so the entry ORDER of the base is
// not accidentally frozen into this test.
func TestContainerShellLayersTheAttemptsEnvironmentOnTopOfItsOwn(t *testing.T) {
	driver := &recordingDriver{}
	// The base, recorded from a call that carries no environment. Read from the driver rather than
	// copied from exec.go, so a change to shellEnv() moves both sides of the comparison together.
	if _, err := newShellExecutor(driver).Run(context.Background(), toolbroker.ShellCommand{
		Argv: []string{"true"}, WorkspaceRoot: "/tmp",
	}); err != nil {
		t.Fatalf("baseline run: %v", err)
	}
	base := driver.specs[0].Env
	if len(base) == 0 {
		t.Fatal("the container's own environment is empty, so the layering assertion below would prove nothing")
	}

	if _, err := newShellExecutor(driver).Run(context.Background(), toolbroker.ShellCommand{
		Argv: []string{"true"}, WorkspaceRoot: "/tmp",
		Env: map[string]string{"JIRA_TOKEN": "value-one", "DATABASE_URL": "value-two"},
	}); err != nil {
		t.Fatalf("layered run: %v", err)
	}
	got := driver.specs[1].Env

	for _, want := range append(append([]string{}, base...), "JIRA_TOKEN=value-one", "DATABASE_URL=value-two") {
		if !contains(got, want) {
			t.Errorf("the container's environment is missing %q; got %v", want, got)
		}
	}
	if len(got) != len(base)+2 {
		t.Errorf("the container's environment has %d entries, want %d (its own %d plus two) — something else was added: %v",
			len(got), len(base)+2, len(base), got)
	}
}

// TestContainerShellRefusesAnEnvironmentKeyThatShadowsItsOwn is the load-bearing one, and it asserts two
// things that must both hold: the call is REFUSED, and NO CONTAINER SPEC IS BUILT. A refusal reported
// after the container ran would be a report, not a refusal.
func TestContainerShellRefusesAnEnvironmentKeyThatShadowsItsOwn(t *testing.T) {
	// Both entries shellEnv() returns, plus a reserved prefix. HOME is as dangerous as PATH here: the
	// container's HOME is the writable workspace mount, and a HOME pointing elsewhere on a read-only
	// rootfs fails every tool that writes a dotfile.
	for _, key := range []string{"PATH", "HOME", "PALAI_SIMCTL_SET", "lowercase", "WITH-DASH", ""} {
		driver := &recordingDriver{}
		_, err := newShellExecutor(driver).Run(context.Background(), toolbroker.ShellCommand{
			Argv: []string{"true"}, WorkspaceRoot: "/tmp",
			Env: map[string]string{key: "attacker-supplied"},
		})
		if err == nil {
			t.Errorf("environment key %q was accepted by the container executor", key)
		}
		if len(driver.specs) != 0 {
			t.Errorf("environment key %q was refused but a container spec was still built: %v", key, driver.specs)
		}
	}
}

// TestContainerShellRedactsTheAttemptsOwnEnvironmentValues covers the result half in the container
// posture: a command that echoes one of its own environment values gets the mask back, on BOTH streams.
func TestContainerShellRedactsTheAttemptsOwnEnvironmentValues(t *testing.T) {
	const value = "jira-token-8f3a2b1c9d"
	driver := &recordingDriver{outcome: oci.Outcome{
		Stdout: []byte("connecting with " + value + "\n"),
		Stderr: []byte("retrying with " + value + "\n"),
	}}
	res, err := newShellExecutor(driver).Run(context.Background(), toolbroker.ShellCommand{
		Argv: []string{"true"}, WorkspaceRoot: "/tmp",
		Env: map[string]string{"JIRA_TOKEN": value},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(res.Stdout, value) || strings.Contains(res.Stderr, value) {
		t.Fatalf("an environment value came back in the result unmasked:\nstdout=%q\nstderr=%q", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "***") || !strings.Contains(res.Stderr, "***") {
		t.Fatalf("the mask is not in the result, so the check above may be passing because nothing was captured:\nstdout=%q\nstderr=%q", res.Stdout, res.Stderr)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
