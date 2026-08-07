// Command palai is the local-stack CLI: it initialises the .palai layout, drives the
// four-service Docker Compose distribution, runs the doctor health surface, stores
// provider credentials, and admits responses over the bootstrap key. Subcommands are
// dispatched by hand over os.Args with stdlib flag sets — no cobra-style dependency.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/palgroup/palai/cmd/cli/internal/admin"
	"github.com/palgroup/palai/cmd/cli/internal/stack"
	"github.com/palgroup/palai/packages/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "palai: "+err.Error())
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	switch args[0] {
	case "init":
		return stack.Init()
	case "up":
		return up(args[1:])
	case "local":
		return local(args[1:])
	case "provider":
		return provider(args[1:])
	case "response":
		return response(args[1:])
	case "config":
		return config(args[1:])
	case "doctor":
		return doctor(args[1:])
	case "support-bundle":
		return supportBundle(args[1:])
	// poolkey joins the admin family for the reason apikey is in it: it is a thin client over one
	// existing endpoint each, and a runner-pool enrolment key is tenancy administration.
	//
	// `pool` joins it for the SAME reason and closes the hole its sibling made visible (E28 T1): until this
	// task there was a verb for a pool's enrolment KEY and none for the pool, so `--pool <pool_id>` could
	// only ever name the one pool a tenant is born with. `palai pool create|list|set-strict`.
	// `org` IS NOT IN THIS LIST, and it was until A.2 Task 6 — which removed the verb from admin.Run's
	// switch and left it here. The two halves disagreed, so `palai org list` reached admin.Run, matched no
	// case, and printed `palai: usage: palai org <>` — a usage line with an EMPTY verb list, which tells an
	// operator nothing and looks like a broken build. Falling through to the full usage below is the
	// answer an unknown command already gets.
	case "apikey", "secret", "pool", "model":
		return admin.Run(args[0], args[1:], os.Stdout, os.Stdin)
	// `palai admin <resource> …` is the explicit spelling of the same family, and the machine lifecycle
	// (E24 T5/T6) is reached ONLY this way — `palai admin runner approve|cordon|resume|revoke|list`. The prefix is not
	// decoration: "runner" is a word this CLI already uses for the process a stack runs
	// (`palai local doctor` reads it, compose names a `runner` service), so a bare `palai runner revoke`
	// would read as an operation on the local container rather than on a fleet member.
	case "admin":
		if len(args) < 2 {
			usage()
			return errors.New("palai admin needs a resource, e.g. `palai admin runner list`")
		}
		return admin.Run(args[1], args[2:], os.Stdout, os.Stdin)
	case "backup":
		return backup(args[1:])
	case "restore":
		return restore(args[1:])
	case "upgrade":
		return upgrade(args[1:])
	case "audit":
		return auditCmd(args[1:])
	case "version":
		fmt.Println(version.Resolve())
		return nil
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// up is the one-command bring-up: read .env.local, store the provider credential, bring the stack
// up on the LIVE selector, and prove it with one real round-trip. It is a top-level verb rather
// than a `local up --verify` flag for two reasons: it does strictly more than compose-up (it writes
// a secret and refuses a fake selector before Docker is touched), and `local up` is what the e2e
// harness drives — a flag that silently changed which adapter that command selects would be the
// same class of surprise this command exists to remove.
func up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	envFile := fs.String("env-file", ".env.local", "dotenv file holding the provider credential (values are never printed or passed in argv)")
	// --native is the Mac deployment (E22 T5): the control plane runs on THIS machine so the agent's
	// shell reaches this machine's toolchain, and only Postgres, the object store and the runner stay
	// in Docker. A flag rather than an env var because it changes where a process runs, which is the
	// kind of thing an operator should be able to read off the command they typed.
	native := fs.Bool("native", false, "run the control plane as a NATIVE process on this machine (default on macOS; postgres/object-store/runner stay in Docker) — docs/operations/palai-on-a-mac.md")
	container := fs.Bool("container", false, "run the control plane in Docker even on macOS — where its shell CANNOT reach this machine's xcodebuild/simctl")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *native && *container {
		return errors.New("--native and --container name where ONE process runs; pass one")
	}
	// ON macOS, NATIVE IS THE DEFAULT, because the flag was asking the operator to know something the
	// machine already knows. A control plane in a container cannot reach this machine's xcodebuild,
	// xcrun or simctl — that is not a limitation of the container, it is what a container IS — so a Mac
	// running the container posture is a Mac that cannot do the one thing a Mac is deployed for.
	//
	// It stays an explicit flag on every other platform: there, a container is the ordinary answer and
	// running the control plane loose on the host is the unusual choice.
	//
	// --container is how a Mac operator says they meant it, and it exists rather than being unreachable
	// because that A/B is exactly what somebody wants when the native path is what broke.
	runNative := *native || (runtime.GOOS == "darwin" && !*container)
	return stack.Bootstrap(*envFile, runNative)
}

func local(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: palai local <up|down|reset|doctor>")
	}
	switch args[0] {
	case "up":
		return stack.Up()
	case "down":
		return stack.Down()
	case "reset":
		fs := flag.NewFlagSet("local reset", flag.ContinueOnError)
		confirm := fs.Bool("confirm", false, "actually delete the stack's data volumes")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return stack.Reset(*confirm)
	case "doctor":
		fs := flag.NewFlagSet("local doctor", flag.ContinueOnError)
		jsonOut := fs.Bool("json", false, "emit the health report as JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return stack.Doctor(*jsonOut)
	default:
		return fmt.Errorf("unknown local subcommand %q", args[0])
	}
}

func provider(args []string) error {
	if len(args) < 2 || args[0] != "add" {
		return errors.New("usage: palai provider add <ref>   (secret value read from stdin)")
	}
	return stack.AddProvider(args[1])
}

// doctor dispatches `palai doctor --env-file production.env` — the health checks against a
// PRODUCTION stack. `palai local doctor` probes host-published ports, which the production overlay
// deliberately does not publish, so it reports almost everything red there for one reason that has
// nothing to do with the stack's health. This reaches the same signals the way that stack can be
// reached: `docker exec` by container name (as backup/restore/support-bundle already do) and the
// TLS edge. See docs/operations/operability.md.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	envFile := fs.String("env-file", "production.env", "the production env file whose PALAI_HOME / PALAI_COMPOSE_PROJECT / PALAI_EDGE_PORT name the stack to check")
	jsonOut := fs.Bool("json", false, "emit the health report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return stack.ProductionDoctor(*envFile, *jsonOut)
}

// config dispatches `palai config validate` — a static, stack-less audit of a production deploy.
func config(args []string) error {
	if len(args) == 0 || args[0] != "validate" {
		return errors.New("usage: palai config validate [--env-file <path>] [--overlay <path>] [--json]")
	}
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	envFile := fs.String("env-file", "deploy/compose/production.env", "production env file to validate")
	// Empty = the overlay this binary would actually bring up: the checkout's committed
	// production.yml, or the copy a packaged binary materialises under ${PALAI_HOME}/compose. A
	// literal repo-relative default only ever resolved from inside a clone.
	overlay := fs.String("overlay", "", "production compose overlay to validate (default: the one this binary would bring up)")
	jsonOut := fs.Bool("json", false, "emit the report as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	return stack.ConfigValidate(*envFile, *overlay, *jsonOut)
}

// supportBundle dispatches `palai support-bundle` — the redacted diagnostics tar.gz.
func supportBundle(args []string) error {
	fs := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	out := fs.String("out", "palai-support-bundle.tar.gz", "output path for the bundle")
	tail := fs.Int("tail", 200, "number of recent log lines per service")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return stack.SupportBundle(*out, *tail)
}

// response has exactly one verb left, and the reason it has ANY is not the CLI's convenience.
//
// `create` was deleted on 2026-08-07: `POST /v1/responses` and the panel already admitted the same
// write, and two truths for one admission is what this component is being taken apart to end. Three
// suites drove it (tests/uat/dr, tests/uat, tests/e2e/local) and all three now POST directly.
//
// `get` STAYS, and not as a leftover. It is the FOURTH CLIENT of the E16 SDK-parity EXIT journey
// (API-012): apps/control-plane/internal/execution/live/sdk_parity_journey_test.go runs
// `go run ./cmd/cli response get <id>` beside the TypeScript, Python and Go SDKs, and
// tests/uat/evidence.go's EqualityClients — {"typescript","python","go","cli"} — is asserted to be
// covered EXACTLY, with an anti-fabrication gate re-canonicalizing all four outputs. Deleting this
// verb means either breaking that journey or cutting a shipped exit claim from four clients to
// three; that is a decision about what replaces the client, and it belongs to the task that removes
// this whole component, not to a slice that is only clearing duplicate write paths.
func response(args []string) error {
	const usage = "usage: palai response get <id>"
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "get":
		if len(args) < 2 || args[1] == "" {
			return errors.New(usage)
		}
		return stack.GetResponse(args[1])
	default:
		return errors.New(usage)
	}
}

// backup drives the installation-level backup: a consistent Postgres dump + object-store copy +
// manifest, written to one archive. Distinct from the RUN-level checkpoint restore (execution/).
func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	out := fs.String("out", "", "archive path (default palai-backup-<project>-<UTC>.tar.gz in cwd)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return stack.InstallBackup(*out)
}

// restore loads an install backup into an EMPTY target stack; `restore verify` checks a restored
// target against its manifest. Both refuse to run without --archive.
func restore(args []string) error {
	verify := false
	if len(args) > 0 && args[0] == "verify" {
		verify = true
		args = args[1:]
	}
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	archive := fs.String("archive", "", "backup archive produced by `palai backup`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *archive == "" {
		return errors.New("usage: palai restore [verify] --archive <path>")
	}
	if verify {
		return stack.InstallRestoreVerify(*archive)
	}
	return stack.InstallRestore(*archive)
}

// upgrade drives the N->N+1 control-plane swap + runner drain + engine-alias roll (§48.4), and
// `upgrade rollback` returns the app image to N on the expanded schema (§48.5). Both read a
// scripts/release/build.sh manifest for the target images.
func upgrade(args []string) error {
	if len(args) > 0 && args[0] == "rollback" {
		fs := flag.NewFlagSet("upgrade rollback", flag.ContinueOnError)
		to := fs.String("to", "", "release manifest of the N (previous) build to roll back to")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *to == "" {
			return errors.New("usage: palai upgrade rollback --to <n-release-manifest.json>")
		}
		return stack.UpgradeRollback(stack.RollbackOptions{To: *to})
	}
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "release manifest of the N+1 target build (scripts/release/build.sh)")
	from := fs.String("from", "", "current running version for the compat check (default: VERSION file)")
	drainRun := fs.String("drain-run", "", "response id to wait terminal before the engine-alias roll")
	drainWait := fs.Duration("drain-wait", 0, "cap on the drain wait (default 90s)")
	skipBackup := fs.Bool("skip-backup", false, "skip the pre-upgrade backup (drill only; never the operator default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifest == "" {
		return errors.New("usage: palai upgrade --manifest <n+1-release-manifest.json> [--from <ver>] [--drain-run <id>] [--skip-backup]")
	}
	return stack.Upgrade(stack.UpgradeOptions{
		Manifest: *manifest, From: *from, DrainRun: *drainRun, DrainWait: *drainWait, SkipBackup: *skipBackup,
	})
}

// auditCmd is the SEC-103 audit-integrity surface (E18 T7). `checkpoint` cuts a signed anchor over
// the events journal; `verify` recomputes the chain from the rows and exits non-zero on any alert.
func auditCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: palai audit <checkpoint|verify>")
	}
	switch args[0] {
	case "checkpoint":
		fs := flag.NewFlagSet("audit checkpoint", flag.ContinueOnError)
		out := fs.String("out", ".", "directory to write the signed checkpoint envelope into")
		key := fs.String("signing-key", "", "release signing key (default PALAI_AUDIT_SIGNING_KEY)")
		allowEmpty := fs.Bool("allow-empty", false,
			"sign a checkpoint over ZERO rows (refused by default: it would anchor the empty prefix and verify green against any journal)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return stack.AuditCheckpoint(*out, *key, *allowEmpty)
	case "verify":
		fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
		cp := fs.String("checkpoint", "", "path to a signed audit-checkpoint.json")
		pub := fs.String("pubkey", "", "trusted public key, obtained OUT OF BAND (default PALAI_AUDIT_PUBKEY)")
		notOlderThan := fs.Duration("not-older-than", 0,
			"raise a `stale` alert when the checkpoint is older than this (rollback: an OLD signed checkpoint still verifies its own prefix)")
		minAnchored := fs.Int("min-anchored", 0,
			"raise a `stale` alert when the checkpoint anchors fewer rows than this (catches a rollback without trusting the clock)")
		jsonOut := fs.Bool("json", false, "emit the typed report as JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return stack.AuditVerify(*cp, *pub, *notOlderThan, *minAnchored, *jsonOut)
	default:
		return fmt.Errorf("usage: palai audit <checkpoint|verify> (got %q)", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `palai — local stack lifecycle

  palai up [--env-file .env.local] [--native]
                                  ONE command: credential -> live stack -> PROVEN real round-trip.
                                  Refuses an unrecognised model selector instead of silently
                                  falling back to the deterministic fake adapter, and fails if it
                                  cannot demonstrate a real provider call.
                                  --native runs the control plane ON THIS MACHINE (the Mac
                                  deployment): its shell then reaches this machine's own
                                  xcodebuild/simctl/axe. Needs PALAI_WORKSPACE_ROOT, and declares
                                  no shell posture of its own — see docs/operations/palai-on-a-mac.md
  palai init                      generate .palai (keys, local CA, ports, config)
  palai local up                  build + start the four-service stack (retains data)
  palai local down                stop the stack, retaining data volumes
  palai local reset --confirm     stop and DELETE the data volumes
  palai local doctor [--json]     run the health checks (15: adds disk/queue/callback/runner_identity)
  palai provider add <ref>        store a provider secret (value on stdin)
  palai response get <id>          retrieve + normalize one response (the SDK-parity fourth client)

operability (E14 T3):
  palai config validate [--env-file <p>] [--overlay <p>] [--json]
                                  static production-posture audit (master key, edge-only surface)
  palai doctor [--env-file <p>] [--json]
                                  health checks against a PRODUCTION stack (18), reached the way
                                  that stack can be: docker-exec by container name + the TLS edge.
                                  "local doctor" probes host ports the production overlay does not
                                  publish; this one asks the same questions over the right
                                  transport, and names any it cannot answer rather than passing it.
  palai support-bundle [--out <p>] [--tail <n>]
                                  redacted diagnostics tar.gz (doctor + compose ps/config/logs)

installation backup/restore (whole-stack; distinct from run-level checkpoints):
  palai backup [--out <path>]              dump Postgres + object store + manifest to one archive
  palai restore --archive <path>           restore into an EMPTY target stack (refuses non-empty)
  palai restore verify --archive <path>    checksum + tenant-id + migration + run-retrieval checks

upgrade (E15 T2; N->N+1 control-plane swap + runner drain + engine-alias roll):
  palai version                            print this binary's build version stamp
  palai upgrade --manifest <n1.json>       backup -> compat verify -> swap -> drain -> engine roll -> smoke
  palai upgrade rollback --to <n.json>     app image back to N (schema stays expanded)

audit integrity (E18 T7 SEC-103; the chain is recomputed FROM THE ROWS, the anchor lives outside the DB):
  palai audit checkpoint --out <dir> [--signing-key <p>]
                                           sign a chain anchor over the events journal (openssl P-256)
  palai audit verify --checkpoint <p> --pubkey <out-of-band p> [--json]
                                           recompute + compare; a gap or tamper alert exits non-zero

admin (thin client over the E13 APIs; base URL + key from flags, env, or .palai):
  palai project create --display-name <n> | list | get <prj_id> | set-policy <prj_id> --allowed-models <a,b>
  palai apikey create --project <prj_id> [--scope <s>]... | list | get <key_id> | revoke <key_id>
  palai secret create --name <n> | list | get <name> | rotate <name>   (secret VALUE on stdin)
  palai poolkey create --pool <pool_id> [--expires-at <rfc3339>] | list [--pool <pool_id>] | revoke <key_id>
                                           runner-pool enrolment keys; create PRINTS the value once
  palai admin runner list | approve <runner_id> | cordon <runner_id> | resume <runner_id> | revoke <runner_id>
                                           ONE machine's lifecycle: cordon stops new leases and keeps
                                           the session, revoke is IRREVERSIBLE and cuts it, approve
                                           admits a machine a STRICT pool is holding (needs the
                                           approve capability, not provision)
`)
}
