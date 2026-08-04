package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/adapters/sandboxes/posture"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestWebhookSecretResolverIsOrgScoped pins F2: the env-key namespace is scoped by org, so a tenant's
// SigningSecretRef can only reach a secret provisioned under its OWN org — naming another org's ref
// resolves to no env var (no cross-tenant HMAC-forgery oracle). The org prefix is server-minted, so it
// is a hard tenant boundary.
func TestWebhookSecretResolverIsOrgScoped(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "b.secret")
	if err := os.WriteFile(secretFile, []byte("whsec_org_b"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	// Only org_b's "shared" ref is bridged.
	t.Setenv("PALAI_WEBHOOK_SECRET_FILE_"+secretEnvKey("org_b")+"__"+secretEnvKey("shared"), secretFile)

	// org_a naming the same ref resolves nothing — it cannot reach org_b's secret.
	if _, err := webhookSecretResolver("org_a", "shared"); err == nil {
		t.Fatal("org_a resolved a secret bridged only under org_b — env namespace is not org-scoped")
	}
	// org_b resolves its own secret.
	got, err := webhookSecretResolver("org_b", "shared")
	if err != nil {
		t.Fatalf("org_b failed to resolve its own secret: %v", err)
	}
	if string(got) != "whsec_org_b" {
		t.Fatalf("resolved secret = %q, want whsec_org_b", got)
	}
}

// TestRemoteToolSecretResolverIsOrgScoped pins the E12 T4 secret hygiene: the remote-tool HMAC secret
// (which signs the outbound invoke AND verifies the inbound callback) is bridged as a FILE PATH under an
// org-scoped, namespace-DISTINCT env key. A tenant's secret_ref can only reach a secret provisioned under
// its OWN org, and the remote-tool namespace never collides with the webhook/inbound ones — the three
// secret sets are non-interchangeable. The raw secret is never an env value, argument, or log line.
func TestRemoteToolSecretResolverIsOrgScoped(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "b.secret")
	if err := os.WriteFile(secretFile, []byte("rtsec_org_b"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	// Only org_b's "sig-ref" is bridged, under the remote-tool namespace.
	t.Setenv("PALAI_REMOTE_TOOL_SECRET_FILE_"+secretEnvKey("org_b")+"__"+secretEnvKey("sig-ref"), secretFile)

	// org_a naming the same ref resolves nothing — it cannot reach org_b's secret.
	if _, err := remoteToolSecretResolver("org_a", "sig-ref"); err == nil {
		t.Fatal("org_a resolved a remote-tool secret bridged only under org_b — env namespace is not org-scoped")
	}
	// A webhook/inbound bridge for the SAME (org, ref) does NOT satisfy the remote-tool resolver (distinct
	// namespaces) — the three secret sets are non-interchangeable.
	t.Setenv("PALAI_WEBHOOK_SECRET_FILE_"+secretEnvKey("org_b")+"__"+secretEnvKey("only-webhook"), secretFile)
	if _, err := remoteToolSecretResolver("org_b", "only-webhook"); err == nil {
		t.Fatal("the remote-tool resolver read a WEBHOOK-namespaced secret — namespaces must be non-interchangeable")
	}
	// org_b resolves its own remote-tool secret from the file (a PATH, never inline bytes).
	got, err := remoteToolSecretResolver("org_b", "sig-ref")
	if err != nil {
		t.Fatalf("org_b failed to resolve its own remote-tool secret: %v", err)
	}
	if string(got) != "rtsec_org_b" {
		t.Fatalf("resolved secret = %q, want rtsec_org_b", got)
	}
}

// TestSecretResolverRejectsAmbiguousOrgKey pins a belt-and-braces guard (E11 T4 residual): an org whose
// normalized env-key form contains the "__" org/ref delimiter would make PALAI_..._SECRET_FILE_<ORG>__<REF>
// ambiguous with a different (org, ref) split, so BOTH secret resolvers reject it rather than resolve a
// colliding key. The org is server-minted (never tenant-forgeable), so this is defence-in-depth on top of
// the org-scoping tenant boundary, not the primary control.
func TestSecretResolverRejectsAmbiguousOrgKey(t *testing.T) {
	const ambiguous = "acme__evil" // normalizes to ACME__EVIL — carries the "__" org/ref delimiter
	for name, resolver := range map[string]func(string, string) ([]byte, error){
		"webhook":     webhookSecretResolver,
		"inbound":     inboundSecretResolver,
		"remote-tool": remoteToolSecretResolver,
	} {
		if _, err := resolver(ambiguous, "shared"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("%s resolver on an ambiguous org key: err = %v, want an 'ambiguous' rejection", name, err)
		}
	}
}

// TestArtifactGCGraceFloorsTinyValue proves a too-small configured grace cannot collapse the
// GC's primary write-safety guard: a typo'd sub-floor value (e.g. "1s") is clamped up to the
// floor, while a value at or above the floor is honored unchanged. Without the floor a live
// in-flight write could be reclaimed before its row commits.
func TestArtifactGCGraceFloorsTinyValue(t *testing.T) {
	if got := artifactGCGrace(time.Second); got != minArtifactGCGrace {
		t.Fatalf("artifactGCGrace(1s) = %s, want the %s floor", got, minArtifactGCGrace)
	}
	if got := artifactGCGrace(minArtifactGCGrace); got != minArtifactGCGrace {
		t.Fatalf("artifactGCGrace(floor) = %s, want %s unchanged", got, minArtifactGCGrace)
	}
	if got := artifactGCGrace(time.Hour); got != time.Hour {
		t.Fatalf("artifactGCGrace(1h) = %s, want 1h honored", got)
	}
}

// TestRepositoryConnectionSecretFailsClosed pins what separates the E13 T9 resolver from its four
// siblings: a repository binding's connection_ref has NO env-file bridge (it is a new consumer, so there
// is no pre-T3 deployment to stay compatible with). With no secret-ref store configured, a binding that
// names a ref resolves NOTHING — and that is an error, never a silent fall-back to the deployment-global
// GitHub App credential the tenant did not choose.
func TestRepositoryConnectionSecretFailsClosed(t *testing.T) {
	if _, err := repositoryConnectionSecret("org_a", "github-conn"); err == nil {
		t.Fatal("resolved a connection ref with no secret store configured; want fail-closed")
	}
	if _, err := repositoryConnectionSecret("", ""); err == nil {
		t.Fatal("resolved an empty org/ref; want an error")
	}
}

// TestMigrateAndExitFlag pins the Kubernetes migration-Job mode selector: the binary enters
// migrate-and-exit ONLY when invoked with --migrate-and-exit, and the flag-less serving path (compose,
// every existing stack) is never mistaken for it. The arg scan ignores unrelated args.
func TestMigrateAndExitFlag(t *testing.T) {
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })

	os.Args = []string{"palai-control-plane"}
	if migrateAndExit() {
		t.Fatal("serving invocation (no args) must NOT select migrate-and-exit")
	}
	os.Args = []string{"palai-control-plane", "--other-flag"}
	if migrateAndExit() {
		t.Fatal("an unrelated arg must NOT select migrate-and-exit")
	}
	os.Args = []string{"palai-control-plane", "--migrate-and-exit"}
	if !migrateAndExit() {
		t.Fatal("--migrate-and-exit must select migrate-and-exit mode")
	}
}

// TestCapabilityWorkerListenerRefusesOffHost pins the posture guard on the capability-worker gateway's
// listener. That listener is CLEARTEXT while its sibling at the SAME topology (startRunnerGateway, a
// port designed to cross a real network) is tls.Listen with ClientCAs — and three things travel on this
// one in the clear: the one-time enrollment token, the workload bearer on EVERY request (no channel
// binding, unlike the runner's client cert whose private key never leaves the runner, so one observed
// request is full worker impersonation for the identity TTL), and the REDEEMED SECRET VALUE in the redeem
// response body. compose.yaml carries `PALAI_RUNNER_LISTEN_ADDR: ":8443"` right there as the pattern to
// copy, and configvalidate.go inspects only host-PUBLISHED ports, so an operator writing
// PALAI_CAPABILITY_WORKER_LISTEN_ADDR=":8444" would otherwise get a wildcard-bound secret-redemption
// endpoint with no warning. So a non-loopback bind is REFUSED, not warned about; loopback (the fixture
// and live paths) keeps working unchanged.
func TestCapabilityWorkerListenerRefusesOffHost(t *testing.T) {
	// Wildcard, routable, and by-name addresses are all refused BEFORE any bind happens.
	for _, addr := range []string{":8444", "0.0.0.0:8444", "[::]:8444", "10.0.0.5:8444", "worker.example.com:8444", "8444", ""} {
		ln, err := listenCapabilityWorker(addr)
		if err == nil {
			_ = ln.Close()
			t.Fatalf("listenCapabilityWorker(%q) bound a cleartext off-host listener; want a refusal", addr)
		}
	}
	// Loopback still binds — the fixture/live worker paths are unchanged.
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		ln, err := listenCapabilityWorker(addr)
		if err != nil {
			t.Fatalf("listenCapabilityWorker(%q) refused a loopback bind: %v", addr, err)
		}
		_ = ln.Close()
	}
}

// TestSlackSocketStartsOnlyWhenTheWorkspaceIsNamed pins the composition-root half of the Slack last
// mile: startSlackSocket is the ONE conditional loop in this binary, and PALAI_SLACK_SOCKET_TEAM_ID
// is what decides. It was untestable-by-omission rather than wrong — compose passed the variable
// nowhere, so the loop was dormant in every deployment an operator actually runs and nothing said
// so. deploy/compose/slack_wiring_test.go holds the other end of the same wire.
//
// Dormant means dormant: a nil drain, so serveWithGracefulDrain has nothing to wait on either.
// TestSlackImageLegIsMountedWhenThereIsAnObjectStore holds the composition the image leg needs, in the
// spirit of TestBareRouterAdvertisesOnlyWhatItCanServe: a capability that is built, tested and then WIRED TO
// NOTHING is this codebase's recurring failure, and its worst property is silence — the owner shared a
// screenshot, the agent said it could not see it, and the only evidence was a parenthetical inside the run's
// own input.
//
// Both directions are asserted and both are load bearing. Mounted-when-there-is-a-store is the regression
// this fixes; unmounted-when-there-is-not is what keeps a deployment with no object store admitting a shared
// file as text instead of failing.
func TestSlackImageLegIsMountedWhenThereIsAnObjectStore(t *testing.T) {
	// The store is a bare value and the pool is nil on purpose: this asserts the MOUNT DECISION and nothing
	// downstream of it. NewWriter dials nothing until a write, and the write path itself is proven against a
	// real object store and a real Postgres in the extensions component suite.
	bridge := mountSlackFileLegs(extensions.NewSlackAdmitter(nil, nil, nil, api.AdmissionLimits{}), &artifacts.Store{}, nil)
	if !bridge.FileFetchReady() {
		t.Fatal("an object store is configured and the Slack image leg is not mounted: every shared screenshot " +
			"is skipped, the run's input says only that a file 'could not be attached', and nothing in the " +
			"control-plane log says why")
	}
	// E22 T5: the OUTBOUND half is mounted by the same decision, and it is asserted here for the same reason
	// the inbound one is — a leg that is built and not mounted fails silently, and this repository has paid
	// for that lesson once already.
	if !bridge.ArtifactUploadReady() {
		t.Fatal("an object store is configured and the Slack artifact upload leg is not mounted: a run's " +
			"screenshot or recording is answered as a link nobody in the thread can open")
	}

	off := mountSlackFileLegs(extensions.NewSlackAdmitter(nil, nil, nil, api.AdmissionLimits{}), nil, nil)
	if off.FileFetchReady() {
		t.Fatal("the image leg reports ready with no object store behind it: a fetched image would have nowhere to go")
	}
	if off.ArtifactUploadReady() {
		t.Fatal("the upload leg reports ready with no object store behind it: there is nothing to read an artifact out of")
	}
}

func TestSlackSocketStartsOnlyWhenTheWorkspaceIsNamed(t *testing.T) {
	// The bridge's store is nil: this asserts the START DECISION, and the loop's own work (which
	// would touch the store) is proven against a real Postgres in the extensions component suite.
	// A panic on the supervised stack is recovered by runGuarded, so it can only cost a backoff.
	bridge := extensions.NewSlackAdmitter(nil, nil, nil, api.AdmissionLimits{})
	supervisor := coordinator.NewSupervisor(func(string, ...any) {}, time.Second)

	t.Setenv("PALAI_SLACK_SOCKET_TEAM_ID", "")
	if drain := startSlackSocket(context.Background(), bridge, supervisor); drain != nil {
		t.Fatal("the Socket Mode loop started with no workspace named: a stack with no Slack app must be unchanged")
	}

	t.Setenv("PALAI_SLACK_SOCKET_TEAM_ID", "T0AMPM5JX8U")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drain := startSlackSocket(ctx, bridge, supervisor)
	if drain == nil {
		t.Fatal("PALAI_SLACK_SOCKET_TEAM_ID names a workspace and the connect loop did not start: nothing can arrive from Slack")
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if err := drain(drainCtx); err != nil {
		t.Fatalf("the started loop did not drain on shutdown: %v", err)
	}
}

// TestShellPostureRefusesBothSandboxImageAndNativeHost pins the mutual exclusion (E22 plan §2). A
// stack runs its shell tool in a container or on the host; there is no "sometimes sandboxed" state,
// because in that state nobody can read off a deployment WHERE a given call ran. Both variables set
// is a configuration mistake with a security consequence, so it is fatal at boot rather than
// resolved by a precedence rule nobody would remember.
func TestShellPostureRefusesBothSandboxImageAndNativeHost(t *testing.T) {
	native, err := posture.Resolve("palai/sandbox@sha256:"+strings.Repeat("a", 64), shellPostureNative)
	if err == nil {
		t.Fatalf("both postures configured and the binary accepted it (native=%v)", native)
	}
	if !strings.Contains(err.Error(), "PALAI_SANDBOX_IMAGE") || !strings.Contains(err.Error(), "PALAI_SHELL_NATIVE") {
		t.Fatalf("the refusal does not name both variables an operator must fix: %v", err)
	}
}

// TestShellPostureAcceptsOnlyTheStringThatSaysWhatItIs is the second half of §2: deleting the
// sandbox must not be reachable by copy-paste. `=1`/`true`/`yes` are what an operator types when
// switching on a feature; `unsandboxed-host` is what an operator types when they have read what it
// means — and it is what `ps` and `docker inspect` then show.
func TestShellPostureAcceptsOnlyTheStringThatSaysWhatItIs(t *testing.T) {
	for _, value := range []string{"1", "true", "yes", "on", "TRUE", "unsandboxed_host", "UNSANDBOXED-HOST", " unsandboxed-host", "unsandboxed-host "} {
		native, err := posture.Resolve("", value)
		if err == nil || native {
			t.Fatalf("PALAI_SHELL_NATIVE=%q was accepted as the unsandboxed host posture", value)
		}
		if !strings.Contains(err.Error(), shellPostureNative) {
			t.Fatalf("the refusal of %q does not name the only accepted value: %v", value, err)
		}
	}
	native, err := posture.Resolve("", shellPostureNative)
	if err != nil || !native {
		t.Fatalf("posture.Resolve(%q) = %v, %v; want the native posture", shellPostureNative, native, err)
	}
	// No posture at all is not an error — it is the default, and it stays that way.
	native, err = posture.Resolve("", "")
	if err != nil || native {
		t.Fatalf("no posture configured = %v, %v; want (false, nil)", native, err)
	}
}

// TestShellRunnerFromEnvKeepsItsNilDiscipline holds the existing contract bit-unchanged across the
// new branch: with no posture configured the function returns nil and the shell tool fails cleanly
// (no runner) rather than escaping. That discipline is what makes an unconfigured deployment safe,
// and a second branch is exactly the kind of change that erodes it.
func TestShellRunnerFromEnvKeepsItsNilDiscipline(t *testing.T) {
	t.Setenv("PALAI_SANDBOX_IMAGE", "")
	t.Setenv("PALAI_SHELL_NATIVE", "")
	if runner := shellRunnerFromEnv(); runner != nil {
		t.Fatalf("no posture configured and a shell runner was bound: %T", runner)
	}
	// A refused value is not a posture either — it must not fall through to the host executor.
	t.Setenv("PALAI_SHELL_NATIVE", "1")
	if runner := shellRunnerFromEnv(); runner != nil {
		t.Fatalf("PALAI_SHELL_NATIVE=1 bound a shell runner: %T", runner)
	}
}

// TestShellRunnerFromEnvBindsTheHostExecutorOnTheNativePosture is the composition-root half of E22
// T1: the declared posture is what selects the runner, and the host executor needs no Docker driver
// to bind — which is the whole point of a Mac deployment.
func TestShellRunnerFromEnvBindsTheHostExecutorOnTheNativePosture(t *testing.T) {
	t.Setenv("PALAI_SANDBOX_IMAGE", "")
	t.Setenv("PALAI_SHELL_NATIVE", shellPostureNative)
	t.Setenv("PALAI_SANDBOX_WALL_TIME", "90s")
	runner := shellRunnerFromEnv()
	if runner == nil {
		t.Fatal("the native posture bound no shell runner — the shell tool would fail on every call")
	}
	if _, ok := runner.(*host.Executor); !ok {
		t.Fatalf("native posture bound %T, want *host.Executor", runner)
	}
}

// TestTheCompositionRootWiresABackgroundRunnerOnTheNativePosture asserts what PRODUCTION wires, not what
// a test can construct. The distinction is not pedantry here: this tree has already shipped a shell wall
// time that was unbounded on one posture and refused every call on the other, invisible to every sandbox
// test because each of them built its own executor and none of them traversed shellRunnerFromEnv.
//
// The claim is the composition root's half of E26 T1: the posture main.go binds is also the thing that
// can start a process the attempt does not own. A stack that bound a shell runner and no background
// runner would accept a `background:true` call and refuse it at dispatch — which is honest, and is also
// exactly the state this test exists to make visible.
func TestTheCompositionRootWiresABackgroundRunnerOnTheNativePosture(t *testing.T) {
	t.Setenv("PALAI_SANDBOX_IMAGE", "")
	t.Setenv("PALAI_SHELL_NATIVE", shellPostureNative)
	t.Setenv("PALAI_SANDBOX_WALL_TIME", "")

	shell := shellRunnerFromEnv()
	if shell == nil {
		t.Fatal("the native posture bound no shell runner")
	}
	background := backgroundRunnerFor(shell)
	if background == nil {
		t.Fatalf("the native posture bound %T as a shell runner but no background runner: every "+
			"background:true call on a Mac deployment would be refused", shell)
	}
	if _, ok := background.(*host.Executor); !ok {
		t.Fatalf("the native posture's background runner is %T, want the same *host.Executor that runs "+
			"synchronous calls — two executors would mean two environments", background)
	}
	// And it is the SAME VALUE, not a second executor built beside it: the environment allow-list, the
	// session directories and the collision refusal are one object's behaviour under both entry points.
	if any(background) != any(shell) {
		t.Fatal("the background runner is a different value from the shell runner; the posture's environment " +
			"would then be built twice and could differ once")
	}
}

// TestABackgroundRunnerIsNotWiredForAPostureThatCannotDetach pins the nil half, and it is the half that
// matters for honesty: an executor that can only run synchronously must leave the seam UNWIRED so the
// tool refuses, rather than being silently downgraded into a blocking call the model believes is
// detached.
func TestABackgroundRunnerIsNotWiredForAPostureThatCannotDetach(t *testing.T) {
	if got := backgroundRunnerFor(syncOnlyRunner{}); got != nil {
		t.Fatalf("a shell runner that cannot detach produced a background runner: %T", got)
	}
}

// syncOnlyRunner implements ShellRunner and nothing else — the shape of any future posture (a relay, a
// remote executor) that can run a command but cannot leave one behind.
type syncOnlyRunner struct{}

func (syncOnlyRunner) Run(context.Context, toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	return toolbroker.ShellResult{}, nil
}

// TestTheNativePostureBoundsAShellCallWithNoEnvironmentSet is one half of the wall-time defect, and it
// runs against the PRODUCTION wiring on purpose. Every sandbox test in this tree builds its own
// oci.Limits or calls host.NewExecutor with an explicit wall time (exec_env_test.go = 1m,
// stdio_component_test.go = 15s, the live MCP suites = 20s/60s), so not one of them ever traversed
// shellRunnerFromEnv's env lookup — a test that builds its own config never sees the config production
// builds, and that is precisely why this shipped.
//
// PALAI_SANDBOX_WALL_TIME is assigned in NO shipped file. Unset, envDuration returns 0, and zero is
// UNBOUNDED for the host executor — so the posture a Mac deployment actually runs under had no wall
// time at all and a hung `xcodebuild` would hold the attempt forever. That the bound is ENFORCED once
// it exists is already proven next door (TestHostShellWallTimeKillsTheWholeProcessGroup, which kills
// the whole process group); what was missing is a composition root that hands it a bound to enforce.
func TestTheNativePostureBoundsAShellCallWithNoEnvironmentSet(t *testing.T) {
	t.Setenv("PALAI_SANDBOX_IMAGE", "")
	t.Setenv("PALAI_SHELL_NATIVE", shellPostureNative)
	// The shipped state: no assignment anywhere. An empty value is what envDuration sees for an unset
	// variable — ParseDuration fails on both — and t.Setenv restores whatever the runner's own
	// environment had.
	t.Setenv("PALAI_SANDBOX_WALL_TIME", "")

	executor, ok := shellRunnerFromEnv().(*host.Executor)
	if !ok {
		t.Fatalf("the native posture bound no host executor: %T", shellRunnerFromEnv())
	}
	if executor.WallTime() <= 0 {
		t.Fatalf("the native posture runs shell commands UNBOUNDED with PALAI_SANDBOX_WALL_TIME unset "+
			"(wall time %v): a hung build holds the attempt forever and nothing reaps it", executor.WallTime())
	}
	if executor.WallTime() != defaultSandboxWallTime {
		t.Fatalf("the native posture defaulted to %v, want %v", executor.WallTime(), defaultSandboxWallTime)
	}
	// An operator who knows their builds are longer must still win — the default is a backstop, not a
	// ceiling on what can be configured.
	t.Setenv("PALAI_SANDBOX_WALL_TIME", "45m")
	executor, _ = shellRunnerFromEnv().(*host.Executor)
	if executor.WallTime() != 45*time.Minute {
		t.Fatalf("an explicit PALAI_SANDBOX_WALL_TIME=45m produced %v", executor.WallTime())
	}
}

// TestTheSandboxPostureAcceptsItsOwnLimitsWithNoEnvironmentSet is the other half, and the same unset
// variable breaks it in the OPPOSITE direction: the OCI driver refuses a non-positive bound before it
// creates anything (Limits.Validate, reached from ContainerSpec.validate at the top of Run), so with
// PALAI_SANDBOX_WALL_TIME unset every containerised shell call was refused the instant it was made.
//
// It asks the driver's OWN check about the limits the COMPOSITION ROOT builds, so it needs no Docker
// daemon and — the point of the exercise — constructs no oci.Limits of its own.
//
// The posture is reached only when an operator sets PALAI_SANDBOX_IMAGE, which is also set in no
// shipped file. That is not a mitigation: enabling the feature was the act that broke it.
func TestTheSandboxPostureAcceptsItsOwnLimitsWithNoEnvironmentSet(t *testing.T) {
	t.Setenv("PALAI_SANDBOX_WALL_TIME", "")

	limits := sandboxLimitsFromEnv()
	if err := limits.Validate(); err != nil {
		t.Fatalf("with PALAI_SANDBOX_WALL_TIME unset the sandbox posture refuses EVERY shell call before "+
			"a container is created: %v (limits %+v)", err, limits)
	}
	// Both postures read the same variable, so they must reach the same bound: a command bounded
	// differently depending on where it ran is the drift this default exists to close.
	if limits.WallTime != defaultSandboxWallTime {
		t.Fatalf("the sandbox posture defaulted to %v, want %v", limits.WallTime, defaultSandboxWallTime)
	}
}

// TestNativeShellPostureDeclarationNamesTheOperatingRule pins the boot line's CONTENT, because a
// declaration that says "native shell enabled" would be worse than none: it announces a feature
// where the truth is a deleted boundary. The line names the posture, what it means for the uid, the
// operating rule, and the measurement that rule comes from — and that citation must resolve in this
// tree.
func TestNativeShellPostureDeclarationNamesTheOperatingRule(t *testing.T) {
	for _, phrase := range []string{"UNSANDBOXED HOST", "uid", "no container boundary", "different customers", "different Macs"} {
		if !strings.Contains(shellPostureDeclaration, phrase) {
			t.Fatalf("the boot declaration does not say %q:\n%s", phrase, shellPostureDeclaration)
		}
	}
	const cited = "docs/research/macos-isolation-without-accounts.md"
	if !strings.Contains(shellPostureDeclaration, cited) {
		t.Fatalf("the boot declaration cites no measurement:\n%s", shellPostureDeclaration)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "..", "..", cited)); err != nil {
		t.Fatalf("the boot declaration cites %s, which does not exist: %v", cited, err)
	}
}

// TestNoDispatchWorkerMeansNoBackgroundExitNotificationEverLands pins E26 §3.6 D14 as a DECLARATION
// rather than leaving it to be discovered by an operator whose build finished and whose model was
// never told.
//
// The behavioural half is in the execution package (TestWithoutTheReconcilerSweepNoBackgroundNoticeEverLands):
// the reconciler's sweep is the ONLY path by which a finished background task reaches a model. This half
// is the composition root's, and it is three facts that have to agree:
//
//	(1) production reads PALAI_DISPATCH_WORKERS through dispatchWorkerCount, and zero means zero;
//	(2) the SHIPPED compose file sets that variable to 0 by default, so the stack most people run is
//	    the stack with no reconciler;
//	(3) the operations document says so in its FIRST paragraph, where a reader meets it.
//
// If any one of the three moves, this test says which.
func TestNoDispatchWorkerMeansNoBackgroundExitNotificationEverLands(t *testing.T) {
	// (1) The gate production actually evaluates, at the two values that matter.
	t.Setenv("PALAI_DISPATCH_WORKERS", "0")
	if got := dispatchWorkerCount(); got > 0 {
		t.Fatalf("dispatchWorkerCount() with PALAI_DISPATCH_WORKERS=0 = %d, want <= 0: startDispatch would build a reconciler", got)
	}
	t.Setenv("PALAI_DISPATCH_WORKERS", "1")
	if got := dispatchWorkerCount(); got <= 0 {
		t.Fatalf("dispatchWorkerCount() with PALAI_DISPATCH_WORKERS=1 = %d, want > 0; the check above would be free", got)
	}

	// (2) The shipped compose file's own value. Read from the file rather than remembered, because the
	// whole point of the declaration is that it is true of what ships.
	compose, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatalf("read the shipped compose file: %v", err)
	}
	if !strings.Contains(string(compose), "PALAI_DISPATCH_WORKERS: ${PALAI_DISPATCH_WORKERS:-0}") {
		t.Fatalf("deploy/compose/compose.yaml no longer defaults PALAI_DISPATCH_WORKERS to 0; the declaration in docs/operations/background-execution.md describes a stack that no longer exists")
	}

	// (3) The document, in its first paragraph — not somewhere in it.
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "operations", "background-execution.md"))
	if err != nil {
		t.Fatalf("read the background execution runbook: %v", err)
	}
	first := firstParagraphAfterTitle(string(doc))
	for _, phrase := range []string{"PALAI_DISPATCH_WORKERS", "0", "no", "notification"} {
		if !strings.Contains(first, phrase) {
			t.Fatalf("the runbook's first paragraph does not mention %q; it reads:\n%s", phrase, first)
		}
	}
}

// firstParagraphAfterTitle returns the first non-heading, non-empty block of a markdown document — the
// paragraph a reader meets.
func firstParagraphAfterTitle(doc string) string {
	var para []string
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, trimmed)
	}
	return strings.Join(para, " ")
}

// THE ENV VAR IS THE GATE, NOT THE FALLBACK — and that is the whole model-connections feature.
//
// modelBrokerFromEnv's own comment says the env selection "is the FALLBACK, not the whole story: a project
// with a published model route dispatches through that route's model and its own connection credential".
// The route half is true. The ADAPTER half was not: the three real families were registered only INSIDE the
// `PALAI_MODEL_PROVIDER == "provider-one"` branch, so on the deployment this feature exists for — a fresh
// self-host whose operator has typed no key into .env.local — the broker knew exactly one adapter, `fake`,
// and a connection created through POST /v1/model-connections routed to modelbroker.ErrUnknownProvider.
//
// A row nobody can dispatch to is the same defect as a row nobody reads. This test is why the adapter map
// is now built OUTSIDE the branch: the env var chooses the deployment DEFAULT ROUTE, never which providers
// this binary can speak to.
//
// It never reaches a network: broker.Route looks the adapter up FIRST and redeems the credential SECOND, so
// an unresolvable secret ref stops the call one step past the assertion this test makes.
func TestModelBrokerSpeaksEveryProviderFamilyOnABootstrapDeployment(t *testing.T) {
	t.Setenv("PALAI_MODEL_PROVIDER", "") // a self-host that has configured nothing
	broker, route, err := modelBrokerFromEnv()
	if err != nil {
		t.Fatalf("modelBrokerFromEnv on a bootstrap deployment = %v, want no error", err)
	}

	// The DEPLOYMENT DEFAULT is unchanged: a stack that configured no provider still runs the
	// deterministic fake, and no existing deployment moves.
	if route.Provider != "fake" || route.Model != "fake" {
		t.Fatalf("deployment default moved: got provider=%q model=%q, want the fake route", route.Provider, route.Model)
	}

	// But a PROJECT that created its own connection routes to a real family, and the broker must know it.
	for _, family := range []string{"provider-one", "provider-two", "openai-compatible"} {
		_, err := broker.Route(context.Background(), family, modelbroker.Request{
			Model:  "any",
			Secret: modelbroker.SecretRef("tenant:org_x/unresolvable"),
		}, nil)
		if errors.Is(err, modelbroker.ErrUnknownProvider) {
			t.Fatalf("a bootstrap deployment cannot dispatch to %q: an operator-created model connection "+
				"naming this family is a row nothing can route to (err=%v)", family, err)
		}
	}
}

// TestEveryDesiredValueThisBinaryAcceptsIsParsedByItsOwnReader is the round trip the desired-configuration
// write path rests on, and it lives HERE — in the composition root — because this is where the real readers
// are. api.DecodeDesiredSettings can assert what it accepts; only this package can assert what the binary
// then does with it.
//
// THE DEFECT IT EXISTS TO PREVENT. Every reader of every catalogued setting COERCES SILENTLY, and each one
// coerces to something different: envInt/envFloat/envDuration to zero, envDurationOr to a named default,
// api.DispatchWorkers to 1. main.go:996 already writes the consequence down for one of them —
// "PALAI_SANDBOX_WALL_TIME=10min (not a Go duration) silently gets the default". A write surface that
// stored `10min` would put a wall time on an operator's screen that the process is not running, which is
// exactly the "declared, and nothing happens" defect the deployment surface was built to expose, shipped
// into the surface itself.
//
// SO IT IS TWO-SIDED, AND THE SECOND SIDE IS THE ONE THAT MATTERS. For each writable setting:
//
//   - ACCEPTED: the decoder takes the value AND this binary's own reader parses it to the stated result.
//     A validator agreeing with a copy of the parsing rules would pass a one-sided test; this one calls the
//     shipped reader.
//   - REFUSED: the decoder rejects the trap AND the reader really would have coerced it, so the refusal is
//     demonstrated to be necessary rather than merely strict. A refusal nobody can show a cost for is how
//     a validator ends up rejecting values operators legitimately type.
func TestEveryDesiredValueThisBinaryAcceptsIsParsedByItsOwnReader(t *testing.T) {
	cases := []struct {
		setting string
		// read renders what THIS BINARY's reader makes of the variable, through the same function
		// production calls. Not a re-implementation: the function itself.
		read func() string
		// accepted is value -> what read() must then return.
		accepted map[string]string
		// refused is a trap value -> what read() would have returned had it been stored, which is what
		// makes refusing it necessary.
		refused map[string]string
	}{
		{
			setting:  "PALAI_DISPATCH_WORKERS",
			read:     func() string { return strconv.Itoa(api.DispatchWorkers()) },
			accepted: map[string]string{"0": "0", "1": "1", "4": "4", "16": "16"},
			// DispatchWorkers coerces to 1, so a stored "four" would run ONE worker while the panel showed four.
			refused: map[string]string{"four": "1", "+4": "4", " 4": "1", "4 ": "1", "0x4": "1"},
		},
		{
			setting:  "PALAI_QUEUE_DEADLINE",
			read:     func() string { return envDuration("PALAI_QUEUE_DEADLINE").String() },
			accepted: map[string]string{"15m": "15m0s", "1h30m": "1h30m0s", "90s": "1m30s"},
			// envDuration coerces to 0, and 0 for this variable means the deadline is DISABLED — so a stored
			// "15 minutes" would turn the queue deadline off while the panel reported fifteen minutes.
			refused: map[string]string{"15 minutes": "0s", "15": "0s", "10min": "0s"},
		},
		{
			setting: "PALAI_FLEET_PARK_TTL",
			read:    func() string { return envDuration("PALAI_FLEET_PARK_TTL").String() },
			// `1h` is the value this deployment sets: three times the six-to-twenty minutes AWS documents for a
			// Mac host to start, so a fleet that is genuinely booting is never cut off, and far under the
			// forty-one hours two runs sat parked on a live stack for want of one.
			accepted: map[string]string{"1h": "1h0m0s", "30m": "30m0s", "6h": "6h0m0s"},
			// `3600` IS THE ONE THAT MATTERS AND IT IS WHY THIS ROW IS HERE. envDuration coerces every parse
			// error to 0, and 0 for this variable means NEVER EXPIRE — so a stored "3600", which is what a
			// person writes for one hour, would silently disable the reaper while the panel reported an hour
			// and two runs waited forever. The grammar refuses it at the write path; this is the other end of
			// the same fact, measured through the reader the binary actually uses.
			refused: map[string]string{"3600": "0s", "1 hour": "0s", "60min": "0s"},
		},
		{
			setting:  "PALAI_RETENTION_STORE_FALSE_TTL",
			read:     func() string { return envDuration("PALAI_RETENTION_STORE_FALSE_TTL").String() },
			accepted: map[string]string{"720h": "720h0m0s", "24h": "24h0m0s"},
			refused:  map[string]string{"30d": "0s", "30 days": "0s"},
		},
		{
			setting:  "PALAI_SANDBOX_WALL_TIME",
			read:     func() string { return sandboxWallTime().String() },
			accepted: map[string]string{"1h30m": "1h30m0s", "30s": "30s"},
			// The one main.go had already written down. envDurationOr falls back to defaultSandboxWallTime.
			refused: map[string]string{"10min": "10m0s", "600": "10m0s"},
		},
		{
			setting:  "PALAI_REQUEST_RATE_PER_SEC",
			read:     func() string { return strconv.FormatFloat(envFloat("PALAI_REQUEST_RATE_PER_SEC"), 'g', -1, 64) },
			accepted: map[string]string{"12.5": "12.5", "100": "100", "0.5": "0.5"},
			// envFloat coerces to 0, and 0 is what api.EdgeLimits reads as UNBOUNDED — so a typo here does not
			// slow the edge down, it removes the limit entirely.
			refused: map[string]string{"12,5": "0", "12.5/s": "0", "many": "0"},
		},
		{
			setting:  "PALAI_REQUEST_BURST",
			read:     func() string { return strconv.Itoa(envInt("PALAI_REQUEST_BURST")) },
			accepted: map[string]string{"40": "40", "1": "1"},
			refused:  map[string]string{"40 requests": "0", "4e1": "0"},
		},
		{
			setting:  "PALAI_MAX_CONCURRENT_RUNS",
			read:     func() string { return strconv.Itoa(envInt("PALAI_MAX_CONCURRENT_RUNS")) },
			accepted: map[string]string{"8": "8", "1": "1"},
			refused:  map[string]string{"eight": "0", "8.0": "0"},
		},
		{
			setting:  "PALAI_MAX_QUEUED_RUNS",
			read:     func() string { return strconv.Itoa(envInt("PALAI_MAX_QUEUED_RUNS")) },
			accepted: map[string]string{"100": "100", "0": "0"},
			refused:  map[string]string{"1_000": "0", "1,000": "0"},
		},
		{
			setting:  "PALAI_RUNNER_CERT_TTL",
			read:     func() string { return envDuration("PALAI_RUNNER_CERT_TTL").String() },
			accepted: map[string]string{"5m": "5m0s", "1h": "1h0m0s"},
			refused:  map[string]string{"5 minutes": "0s", "300": "0s"},
		},
		{
			setting: "PALAI_MODEL_PROVIDER",
			// The reader is an EQUALITY, not a parse (main.modelBrokerFromEnv:810), so what this renders is
			// the branch the binary takes — which is the fact an operator setting this cares about.
			read: func() string {
				if os.Getenv("PALAI_MODEL_PROVIDER") == "provider-one" {
					return "live"
				}
				return "fake"
			},
			accepted: map[string]string{"provider-one": "live", "fake": "fake"},
			// `provider one` and `Provider-One` are the shape of the trap this whole tree names: any value
			// other than the exact string falls through to the deterministic fake adapter, whose output renders
			// exactly like a real answer. The grammar cannot refuse `Provider-One` (it is a legal token and a
			// legal provider name for a future adapter), so only the space form is listed.
			refused: map[string]string{"provider one": "fake", "provider-one\n": "fake"},
		},
		{
			setting: "PALAI_TOOL_ERROR_BUDGET",
			read:    func() string { return strconv.Itoa(execution.ToolErrorBudget()) },
			// `0` is unbounded and reads back as 0 — an operator typed it. Everything unparseable falls back
			// to 16 rather than to infinity, which is the direction that matters: a typo must not remove a
			// ceiling. The grammar refuses the two shapes a human actually produces.
			accepted: map[string]string{"32": "32", "0": "0"},
			refused:  map[string]string{"sixteen": "16", "16 ": "16"},
		},
		{
			setting:  "PALAI_MODEL",
			read:     func() string { return os.Getenv("PALAI_MODEL") },
			accepted: map[string]string{"gpt-4o-mini": "gpt-4o-mini", "anthropic/claude-x": "anthropic/claude-x"},
			refused:  map[string]string{"gpt-4o mini": "gpt-4o mini", "${PALAI_SECRET_PROVIDER_ONE}": "${PALAI_SECRET_PROVIDER_ONE}"},
		},
	}

	// Every writable setting must appear, or the table has quietly stopped covering one.
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.setting] = true
	}
	// CONTROL-PLANE WRITABLES ONLY, and the narrowing is the honest half rather than a loophole: this table
	// round-trips a value through THIS BINARY's reader, and a runner-plane setting is read by cmd/runner.
	// Asserting it here would measure the wrong process. Its own guard is TestThePlaneWinsOverTheBoxsOwn
	// Environment in cmd/runner, which drives the reader that actually takes it.
	for _, name := range api.DesiredWritableSettingsFor(api.ControlPlaneName) {
		if !covered[name] {
			t.Errorf("%s is writable from the panel and this round trip does not cover it. An accepted value whose "+
				"reader nobody checked is the whole defect: the panel shows one number and the process runs another", name)
		}
	}

	for _, tc := range cases {
		t.Run(tc.setting, func(t *testing.T) {
			for value, want := range tc.accepted {
				body := `{"settings":{"` + tc.setting + `":` + strconv.Quote(value) + `}}`
				if _, err := api.DecodeDesiredSettings([]byte(body)); err != nil {
					t.Errorf("the write surface refuses %q, which this binary parses fine: %v", value, err)
					continue
				}
				t.Setenv(tc.setting, value)
				if got := tc.read(); got != want {
					t.Errorf("%s=%q: the panel stores and shows %q, this binary reads it as %q, and the table says it "+
						"should read as %q. The write surface and the reader disagree about the same string",
						tc.setting, value, value, got, want)
				}
			}
			for value, coerced := range tc.refused {
				body := `{"settings":{"` + tc.setting + `":` + strconv.Quote(value) + `}}`
				if _, err := api.DecodeDesiredSettings([]byte(body)); err == nil {
					t.Errorf("the write surface ACCEPTS %s=%q, and this binary reads it as %q — a panel showing the "+
						"stored value would be showing something the process is not running", tc.setting, value, coerced)
				}
				t.Setenv(tc.setting, value)
				if got := tc.read(); got != coerced {
					t.Errorf("%s=%q reads as %q, but the refusal above is justified on it reading as %q. If the reader "+
						"stopped coercing, the refusal may no longer be buying anything", tc.setting, value, got, coerced)
				}
			}
		})
	}
}

// TestDispatchRefusesTheAssignmentOnlyHandlerAndNamesTheMissingWire is the composition-root half of the
// stranded-run defect measured on a live stack on 2026-08-02.
//
// `startDispatch` built `execution.AdvanceRun(spine)` and replaced it with the real exec-path handler
// only INSIDE `if gateway != nil`. AdvanceRun drives a run queued -> provisioning -> running and returns
// SUCCESS; it opens no attempt and dials nothing. So a control plane whose PALAI_RUNNER_LISTEN_ADDR was
// unset, with dispatch on, answered every response.run job by marking its run `running` and completing
// the job. The durable record names the exact run it happened to: of 240 jobs on that stack, 227 carried
// result_hash `run:<id>:executed` and exactly ONE carried `run:<id>:assigned` — and that run had been
// `running` with no attempt row, no engine and no terminal for over an hour.
//
// THE COMMENT ABOVE startDispatch CLAIMED A DRIVER THAT DOES NOT EXIST. It said the assignment-only
// behaviour is what "the read-path SSE e2e drives". Measured: scripts/test/e2e sets
// PALAI_DISPATCH_WORKERS=0, and its own comment says it runs "without a dispatcher so no background
// worker races those manual transitions" — so dispatchWorkerCount()'s early return fires and AdvanceRun
// is never constructed. No shipped tier drives it. That is what makes refusing safe rather than a
// trade: the branch had no user, only a victim.
//
// The posture is now a DECISION with a name. A stack that cannot execute a run leaves it `queued` —
// recoverable the moment a properly-wired control plane starts — instead of claiming an assignment that
// never happened.
func TestDispatchRefusesTheAssignmentOnlyHandlerAndNamesTheMissingWire(t *testing.T) {
	// Wired: the gateway is bound, so dispatch starts and drives the real exec path.
	if start, refusal := dispatchPosture(1, true); !start || refusal != "" {
		t.Fatalf("dispatchPosture(1, bound) = (%v, %q), want (true, \"\") — a wired stack must dispatch", start, refusal)
	}
	// Off: zero workers is zero, and that is not a refusal to explain.
	if start, refusal := dispatchPosture(0, false); start || refusal != "" {
		t.Fatalf("dispatchPosture(0, unbound) = (%v, %q), want (false, \"\") — dispatch is simply off", start, refusal)
	}
	// THE DEFECT: workers asked for, no runner listener bound.
	start, refusal := dispatchPosture(1, false)
	if start {
		t.Fatal("dispatchPosture(1, unbound) starts dispatch: the assignment-only handler marks each run `running` and completes its job, so the run has no attempt, no engine and no terminal, and nothing in the tree will ever move it")
	}
	// BOTH EXITS, and the second is not decoration. An API-only control plane is a posture this tree
	// already ships — scripts/test/e2e drives the read path with PALAI_DISPATCH_WORKERS=0, and
	// deploy/compose/compose.env.example defaults to 0 — so a refusal that named only the listener would
	// read as "you must run a runner plane", which is false, and would send an operator to configure
	// certificates for a stack that has no runs to execute.
	for _, wanted := range []string{"PALAI_RUNNER_LISTEN_ADDR", "PALAI_DISPATCH_WORKERS=0"} {
		if !strings.Contains(refusal, wanted) {
			t.Fatalf("the refusal is %q and does not name %s: it must give an operator BOTH resolutions — bind a listener, or say this control plane is deliberately API-only", refusal, wanted)
		}
	}
}

// TestEveryShippedControlPlaneBringUpBindsARunnerListener is the reachability half of the refusal
// above, and it is what makes that refusal safe to ship rather than merely strict.
//
// Measured 2026-08-03 across every shipped way to start a control plane:
//
//   - deploy/compose/compose.yaml sets PALAI_RUNNER_LISTEN_ADDR unconditionally, which covers
//     scripts/test/upgrade-drill.sh and scripts/package/runner/splitvm-proof.sh — both set
//     PALAI_DISPATCH_WORKERS>0 and both launch through `docker compose` with that file.
//   - deploy/helm/palai/templates/deployment.yaml sets it.
//   - `palai local up` sets it for every native stack: cmd/cli/internal/stack/native.go's
//     nativeRunnerListen treats UNSET as the normal case and returns ":<PALAI_RUNNER_PORT>", and the
//     child environment always carries the result. That covers scripts/ops/record-runbook-transcripts.sh
//     and the scripts/uat/* flows.
//
// So no shipped posture reaches the refusal. The only way to reach it is to run the binary directly
// with the variable unset — which is exactly what stranded six runs on a live stack.
//
// This test guards the ONE file that could silently change that: a compose file that stopped setting the
// listener would turn every `docker compose` bring-up with workers into a refusing control plane, and the
// first symptom would be runs that never leave `queued`.
func TestEveryShippedControlPlaneBringUpBindsARunnerListener(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	for _, f := range []struct{ path, want string }{
		{filepath.Join(root, "deploy", "compose", "compose.yaml"), "PALAI_RUNNER_LISTEN_ADDR"},
		{filepath.Join(root, "deploy", "helm", "palai", "templates", "deployment.yaml"), "PALAI_RUNNER_LISTEN_ADDR"},
		// The CLI's own sentence that unset is normal and yields a bound address. Asserted on the
		// comment-free code line so a reworded comment does not satisfy it.
		{filepath.Join(root, "cmd", "cli", "internal", "stack", "native.go"), `"PALAI_RUNNER_LISTEN_ADDR": listen,`},
	} {
		body, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("read %s: %v", f.path, err)
		}
		if !strings.Contains(string(body), f.want) {
			t.Fatalf("%s no longer carries %q: a shipped bring-up that stops binding a runner listener now REFUSES to dispatch, and its runs sit in `queued` with the reason only in the log", f.path, f.want)
		}
	}
}

// THE DEPLOYMENT SEAM FOR THE DETERMINISTIC ADAPTER, driven through the reader production calls.
//
// IT EXISTS BECAUSE A STACK WITH NO CREDENTIAL COULD PROVE NO TOOL PATH AT ALL. The shipped fake answers
// a fixed script with no tool calls and nothing could point it elsewhere, so every end-to-end proof that a
// tool ran on a machine needed a provider key — and this tree exposes no tools to a real provider, so the
// key would not have produced a tool call either.
//
// All three legs go through modelBrokerFromEnv and then through broker.Route, because the claim is about
// what a RUN gets, and a run gets the adapter the broker dispatches to. Building a fake.Adapter here would
// assert the package, not the deployment.
func TestTheDeploymentCanRouteTheDeterministicAdapterAScript(t *testing.T) {
	// LEG 1 — UNROUTED: byte-for-byte what every deployment that has ever existed answers. `fake-local`
	// and `ok` are read back out of committed model steps by the wiring and UAT receipts, so this is the
	// leg that must not move.
	t.Run("unset replays the built-in script", func(t *testing.T) {
		t.Setenv("PALAI_MODEL_PROVIDER", "")
		t.Setenv(fakeScriptFileEnv, "")
		broker, route, err := modelBrokerFromEnv()
		if err != nil {
			t.Fatalf("modelBrokerFromEnv = %v, want the bootstrap deployment", err)
		}
		res, err := broker.Route(context.Background(), route.Provider, modelbroker.Request{
			ModelRequestID: "mreq_default", Secret: route.Secret,
		}, nil)
		if err != nil {
			t.Fatalf("Route = %v, want the built-in script", err)
		}
		if res.Output != "ok" || res.ProviderRequestID != "fake-local" || res.Model != "fake" {
			t.Fatalf("the unrouted answer moved: output=%q provider_request_id=%q model=%q, want ok/fake-local/fake",
				res.Output, res.ProviderRequestID, res.Model)
		}
		if len(res.ToolCalls) != 0 {
			t.Fatalf("the unrouted answer now calls %d tool(s); every deployment that set nothing must answer "+
				"without reaching one", len(res.ToolCalls))
		}
	})

	// LEG 2 — ROUTED: a file on disk, and a run that calls a tool and then answers with what it learned.
	t.Run("a routed file drives a tool call and its answer", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "uname.json")
		if err := os.WriteFile(path, []byte(`{
		  "provider_request_id": "scripted-uname",
		  "model": "fake",
		  "tool_calls": [{"id": "call_uname", "name": "palai.workspace.shell", "arguments": "{\"command\":\"uname\"}"}],
		  "then": [{"provider_request_id": "scripted-answer", "model": "fake", "output": "Darwin"}]
		}`), 0o600); err != nil {
			t.Fatalf("write script: %v", err)
		}
		t.Setenv("PALAI_MODEL_PROVIDER", "")
		t.Setenv(fakeScriptFileEnv, path)

		broker, route, err := modelBrokerFromEnv()
		if err != nil {
			t.Fatalf("modelBrokerFromEnv = %v, want the routed script accepted", err)
		}
		first, err := broker.Route(context.Background(), route.Provider, modelbroker.Request{
			ModelRequestID: "mreq_1", Secret: route.Secret,
			Tools:    []modelbroker.ToolSchema{{Name: "palai.workspace.shell"}},
			Messages: []modelbroker.Message{{Role: "user", Content: "which kernel?"}},
		}, nil)
		if err != nil {
			t.Fatalf("first step = %v, want the scripted tool call", err)
		}
		if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "palai.workspace.shell" ||
			first.ToolCalls[0].Arguments != `{"command":"uname"}` {
			t.Fatalf("first step tool calls = %+v, want the shell call the file scripts: a credential-less "+
				"stack still cannot drive a tool", first.ToolCalls)
		}

		second, err := broker.Route(context.Background(), route.Provider, modelbroker.Request{
			ModelRequestID: "mreq_2", Secret: route.Secret,
			Tools: []modelbroker.ToolSchema{{Name: "palai.workspace.shell"}},
			Messages: []modelbroker.Message{
				{Role: "user", Content: "which kernel?"},
				{Role: "assistant", ToolCalls: first.ToolCalls},
				{Role: "tool", ToolCallID: "call_uname", Content: "Darwin"},
			},
		}, nil)
		if err != nil {
			t.Fatalf("second step = %v, want the scripted answer", err)
		}
		if second.Output != "Darwin" || len(second.ToolCalls) != 0 || second.ProviderRequestID != "scripted-answer" {
			t.Fatalf("second step output=%q tool_calls=%d id=%q, want the follow-up turn's answer",
				second.Output, len(second.ToolCalls), second.ProviderRequestID)
		}
	})

	// LEG 3 — REFUSED: a routing that cannot be honoured stops the boot instead of quietly leaving the
	// built-in script in place. A silent fallback is an operator watching a run that calls nothing while
	// believing their file is driving it — which is the belief this whole seam exists to make impossible.
	t.Run("a routing that cannot be honoured refuses", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-such-script.json")
		empty := filepath.Join(t.TempDir(), "empty.json")
		if err := os.WriteFile(empty, []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write script: %v", err)
		}
		for _, c := range []struct{ name, provider, path, want string }{
			{"a path with no file behind it", "", missing, "read scripted exchange"},
			{"a script that answers nothing", "", empty, "answers nothing"},
			// The deterministic adapter is reachable ONLY as the deployment default — no connection can
			// name the family — so a script routed into a live deployment is replayed by nothing at all.
			{"a live deployment", "provider-one", empty, "PALAI_MODEL_PROVIDER"},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Setenv("PALAI_MODEL_PROVIDER", c.provider)
				t.Setenv(fakeScriptFileEnv, c.path)
				broker, _, err := modelBrokerFromEnv()
				if err == nil {
					t.Fatalf("modelBrokerFromEnv accepted %s and returned a broker (%p): the deployment would "+
						"run the built-in script while its operator believes the routed one is driving", c.name, broker)
				}
				if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), fakeScriptFileEnv) {
					t.Fatalf("refusal = %v, want it to name %s and %q", err, fakeScriptFileEnv, c.want)
				}
			})
		}
	})
}
