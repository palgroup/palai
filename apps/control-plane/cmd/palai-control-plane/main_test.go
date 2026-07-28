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
