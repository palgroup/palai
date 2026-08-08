package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestThePlaneWinsOverTheBoxsOwnEnvironment is the runner half of the desired-configuration round trip, and
// it exists because the control-plane binary's own round-trip guard structurally cannot cover it: that guard
// pushes a value through THIS setting's reader, and this setting's reader is in this binary, not that one.
// Narrowing it there (DesiredWritableSettingsFor) without writing this here would have moved a setting out
// of one guard's coverage and into nobody's — which is the shape this tree keeps finding.
//
// THE PRECEDENCE IS THE PRODUCT DECISION. The plane wins because the point of the runner_pool document is
// that an operator configures a fleet from one screen instead of editing a file on every box. The
// environment is still read second, and that second read is the migration path rather than a fallback worth
// keeping forever: the compose file, the Helm chart and the systemd unit all set this variable today, so a
// runner that ignored them would come up misconfigured against a control plane nobody had given a document.
func TestThePlaneWinsOverTheBoxsOwnEnvironment(t *testing.T) {
	const name = "PALAI_RUNNER_CONCURRENCY"

	t.Run("the plane's value is used even when the box disagrees", func(t *testing.T) {
		t.Setenv(name, "1")
		if got := planeIntDefault(map[string]string{name: "4"}, name, 1); got != 4 {
			t.Fatalf("concurrency = %d, want 4 — the box's own environment overrode the admin plane, so an "+
				"operator's fleet-wide decision would not reach a machine that had the variable set locally", got)
		}
	})

	t.Run("no document leaves the box's environment in charge", func(t *testing.T) {
		t.Setenv(name, "3")
		if got := planeIntDefault(nil, name, 1); got != 3 {
			t.Fatalf("concurrency = %d, want 3 — a pool nobody has configured must behave exactly as it did "+
				"before the document existed, or this change breaks every deployment built before it", got)
		}
	})

	t.Run("neither means the built-in default", func(t *testing.T) {
		t.Setenv(name, "")
		if got := planeIntDefault(nil, name, 1); got != 1 {
			t.Fatalf("concurrency = %d, want the built-in 1", got)
		}
	})

	// A VALUE THE PLANE SENT THAT DOES NOT PARSE FALLS TO THE ENVIRONMENT, NOT TO THE DEFAULT, and the
	// difference is what an operator sees. Falling to the default would make a typo in the panel
	// indistinguishable from an unconfigured pool: the machine would quietly serve one lease and the screen
	// would show four. Falling to the environment keeps the machine on whatever it was already doing, which
	// is the answer that does not invent a number nobody chose.
	t.Run("an unparseable plane value falls through to the environment", func(t *testing.T) {
		t.Setenv(name, "3")
		for _, bad := range []string{"four", "", "0", "-2", " 4"} {
			if got := planeIntDefault(map[string]string{name: bad}, name, 1); got != 3 {
				t.Errorf("plane value %q gave %d, want the environment's 3 — a typo in the panel must not be "+
					"silently indistinguishable from a pool nobody configured", bad, got)
			}
		}
	})
}

// TestAMachineDeclaresItsOwnCeiling covers the link that did not exist: `runners.capacity` was declarable
// on the wire (packages/runner enrollmentRequest), read by the enrolment handler, written by the store and
// enforced by coordinator.AcquireLease — and NOTHING IN THIS BINARY FILLED IT. Measured 2026-08-05 on a live
// stack: 47 machines, one distinct capacity, `0`. Since AcquireLease guards its ceiling with `capacity > 0`,
// ErrMachineAtCapacity could not fire in any deployment, and the two `awaiting_capacity` attempts on that
// stack were the NULL-pool park (placement.go), never the ceiling.
//
// The value is read here rather than from the admin plane's settings document because it is needed at
// ENROLMENT, and the document arrives in the enrolment RESPONSE — a ceiling delivered after the fact cannot
// bound the enrolment that carried it.
func TestAMachineDeclaresItsOwnCeiling(t *testing.T) {
	const name = "PALAI_RUNNER_CAPACITY"

	// UNSET DECLARES NOTHING, and this leg is the compatibility claim: `omitempty` keeps the field off the
	// wire entirely, so a machine nobody configured sends the bytes it always sent and enrols with no ceiling.
	t.Run("unset declares nothing", func(t *testing.T) {
		t.Setenv(name, "")
		if got := declaredCapacity(); got != 0 {
			t.Fatalf("capacity = %d, want 0 — an unconfigured machine must enrol exactly as it did before this "+
				"reader existed, or every deployment gains a ceiling nobody chose", got)
		}
	})

	t.Run("a number is what the machine declares", func(t *testing.T) {
		t.Setenv(name, "1")
		if got := declaredCapacity(); got != 1 {
			t.Fatalf("capacity = %d, want 1", got)
		}
	})

	// A NEGATIVE IS PASSED THROUGH, NOT CLAMPED, and that is the whole reason this is not envIntDefault.
	// The control plane refuses it by name (fleet.ErrCapacityNotDeclarable → 400 "declared capacity cannot
	// be negative"); clamping to 0 here would make that shipped refusal unreachable from the only binary
	// that can reach it — the same "declared but nothing writes it" defect this reader exists to fix.
	t.Run("a negative reaches the control plane's refusal", func(t *testing.T) {
		t.Setenv(name, "-2")
		if got := declaredCapacity(); got != -2 {
			t.Fatalf("capacity = %d, want -2 — clamping here leaves ErrCapacityNotDeclarable with no caller", got)
		}
	})

	t.Run("an unparseable value declares nothing", func(t *testing.T) {
		t.Setenv(name, "four")
		if got := declaredCapacity(); got != 0 {
			t.Fatalf("capacity = %d, want 0", got)
		}
	})
}

// TestOneAddressDerivesWhatFourVariablesUsedToCarry is the "one CLI" half of the install: a machine
// joining a fleet knows WHERE its control plane is, and should not separately be told three URL paths and
// a DNS name on that same host. Asking for them is how a one-command install becomes a file to edit on
// every box — the thing the runner-plane document exists to remove, applied to the bootstrap itself.
//
// EXPLICIT ALWAYS WINS, and that is not politeness: compose, Helm and the systemd unit all set these four
// today, so a derivation that overrode them would silently repoint every existing deployment at whatever
// PALAI_CONTROLLER_URL happened to say.
func TestOneAddressDerivesWhatFourVariablesUsedToCarry(t *testing.T) {
	const base = "https://cp.example.internal:8443"

	// ‼️ EACH CASE CARRIES THE DERIVATION loadConfig PASSES, NOT ONE THE TEST CHOOSES. The table used to
	// hand `joinPath(tc.path)` to every name, which measured the helper and not the wiring — and that is
	// how the session URL was derived as `https://…/v1/runner/connect` for as long as it was: the session
	// refuses anything that is not wss, so every deployment had to be handed a pre-built value by a shell
	// bridge, and three scripts spelled the swap while no Go code did.
	t.Run("the paths are the gateway's own routes, and the session is wss", func(t *testing.T) {
		for _, tc := range []struct {
			name, want string
			derive     func(string) string
		}{
			{"PALAI_ENROLLMENT_URL", base + "/v1/runner/enroll", joinPath("/v1/runner/enroll")},
			{"PALAI_SESSION_URL", "wss://cp.example.internal:8443/v1/runner/connect", outboundSessionURL},
			{"PALAI_RENEW_URL", base + "/v1/runner/renew", joinPath("/v1/runner/renew")},
			{"PALAI_SETTINGS_URL", base + "/v1/runner/settings", joinPath("/v1/runner/settings")},
		} {
			t.Setenv(tc.name, "")
			if got := derivedEnv(tc.name, base, tc.derive); got != tc.want {
				t.Errorf("%s derived %q, want %q", tc.name, got, tc.want)
			}
		}
	})

	// ‼️ AND THIS ONE DRIVES loadConfig ITSELF, because the table above still does not. Naming the right
	// derivation in a test's own table proves the table: reverting loadConfig to joinPath left every case
	// above GREEN, measured 2026-08-06. The only assertion that can see that revert is one that calls the
	// function production calls and reads what it returns.
	t.Run("loadConfig itself returns a wss session URL", func(t *testing.T) {
		t.Setenv("PALAI_CONTROLLER_URL", base)
		t.Setenv("PALAI_ENROLLMENT_TOKEN", "rpk_test")
		for _, name := range []string{"PALAI_SESSION_URL", "PALAI_ENROLLMENT_URL", "PALAI_RENEW_URL", "PALAI_SETTINGS_URL", "PALAI_CONTROLLER_CA"} {
			t.Setenv(name, "")
		}
		_, _, sessionURL, _, _, _, _, _ := loadConfig(nil)
		if !strings.HasPrefix(sessionURL, "wss://") {
			t.Fatalf("loadConfig derived session URL %q; the session refuses anything that is not wss, so this machine would retry forever while looking healthy in Fleet", sessionURL)
		}
	})

	// A PORT IN A ServerName IS A HANDSHAKE THAT NEVER MATCHES. The DNS name is what the runner pins its
	// TLS to and what the certificate's SAN carries, so the host must come out without the port.
	t.Run("the DNS name drops the port", func(t *testing.T) {
		t.Setenv("PALAI_CONTROLLER_DNS", "")
		if got := derivedEnv("PALAI_CONTROLLER_DNS", base, hostOf); got != "cp.example.internal" {
			t.Fatalf("controller DNS = %q, want the host with no port — a ServerName carrying a port never "+
				"matches the certificate's SAN", got)
		}
	})

	t.Run("an explicit value beats the derivation", func(t *testing.T) {
		t.Setenv("PALAI_SESSION_URL", "https://old.internal/v1/runner/connect")
		got := derivedEnv("PALAI_SESSION_URL", base, joinPath("/v1/runner/connect"))
		if got != "https://old.internal/v1/runner/connect" {
			t.Fatalf("derived %q over an explicit value; every deployment that sets these four today would be "+
				"silently repointed at whatever PALAI_CONTROLLER_URL says", got)
		}
	})

	t.Run("a trailing slash does not double up", func(t *testing.T) {
		if got := joinPath("/v1/runner/enroll")(base + "/"); got != base+"/v1/runner/enroll" {
			t.Fatalf("joined to %q; an operator pasting an address with a trailing slash is not an error", got)
		}
	})

	// The id a machine SENDS is a label — the control plane mints the real one — so defaulting it costs a
	// confusing row at worst, never a wrong identity. Requiring it costs an operator at a terminal.
	t.Run("the machine names itself when nobody named it", func(t *testing.T) {
		t.Setenv("PALAI_RUNNER_ID", "")
		if got := defaultEnv("PALAI_RUNNER_ID", machineName); got == "" {
			t.Fatal("a machine with no configured id has no default name, so enrolment fails on a label")
		}
		t.Setenv("PALAI_RUNNER_ID", "explicit-name")
		if got := defaultEnv("PALAI_RUNNER_ID", machineName); got != "explicit-name" {
			t.Fatalf("id = %q, want the explicit value", got)
		}
	})
}

// TestTheDeviceBinaryAnswersVersionInsteadOfBecomingAnAgent — THE INSTALLER RUNS THIS BINARY, AND
// EVERYTHING THAT IS NOT A SUBCOMMAND IS THE AGENT.
//
// install.sh decides whether it already has the build it is about to write by running
// `"$dest" --version` and comparing. Everything after the subcommand dispatch in main is the agent,
// which runs until it is stopped — so a binary that fell through on `--version` started a SECOND agent
// and never returned, and the installer hung mid-line. The first install on a machine worked because
// there was nothing to probe; every re-install after it hung, which is the shape a provisioner meets on
// its second boot rather than its first. Measured on a real Mac on 2026-08-07.
func TestTheDeviceBinaryAnswersVersionInsteadOfBecomingAnAgent(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "palai")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the device binary: %v\n%s", err, out)
	}
	// The FLAGS install.sh could plausibly use, all three, because the installer is a shell script this
	// package does not own and a contract that holds for one spelling is not a contract.
	for _, flag := range []string{"--version", "-version", "version"} {
		t.Run(flag, func(t *testing.T) {
			// ‼️ THE BOUND IS GENEROUS ON PURPOSE, AND 15s PRODUCED A FALSE RED. The property this test
			// owns is "`--version` RETURNS rather than becoming an agent", and the failure it guards
			// never returns at all — so any finite deadline proves it. A tight one instead measures the
			// machine: on 2026-08-08 this failed inside `make verify` at a fifteen-minute load average of
			// 47, and passed in 2.6s on the same commit when run alone. The red said "it fell through to
			// the agent", which is a frightening and completely wrong diagnosis.
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, bin, flag).CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("`palai %s` did not return — it fell through to the agent, and an installer probing "+
					"this binary hangs forever", flag)
			}
			if err != nil {
				t.Fatalf("`palai %s` failed: %v\n%s", flag, err, out)
			}
			if strings.TrimSpace(string(out)) == "" {
				t.Fatalf("`palai %s` printed nothing — install.sh compares this to a version and would "+
					"reinstall on every boot", flag)
			}
		})
	}
}
