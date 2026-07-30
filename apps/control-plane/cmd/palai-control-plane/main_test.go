package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/artifacts"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/coordinator"
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
	native, err := resolveShellPosture("palai/sandbox@sha256:"+strings.Repeat("a", 64), shellPostureNative)
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
		native, err := resolveShellPosture("", value)
		if err == nil || native {
			t.Fatalf("PALAI_SHELL_NATIVE=%q was accepted as the unsandboxed host posture", value)
		}
		if !strings.Contains(err.Error(), shellPostureNative) {
			t.Fatalf("the refusal of %q does not name the only accepted value: %v", value, err)
		}
	}
	native, err := resolveShellPosture("", shellPostureNative)
	if err != nil || !native {
		t.Fatalf("resolveShellPosture(%q) = %v, %v; want the native posture", shellPostureNative, native, err)
	}
	// No posture at all is not an error — it is the default, and it stays that way.
	native, err = resolveShellPosture("", "")
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
