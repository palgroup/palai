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
	case "support-bundle":
		return supportBundle(args[1:])
	case "org", "project", "apikey", "secret":
		return admin.Run(args[0], args[1:], os.Stdout, os.Stdin)
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	return stack.Bootstrap(*envFile)
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

// config dispatches `palai config validate` — a static, stack-less audit of a production deploy.
func config(args []string) error {
	if len(args) == 0 || args[0] != "validate" {
		return errors.New("usage: palai config validate [--env-file <path>] [--overlay <path>] [--json]")
	}
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	envFile := fs.String("env-file", "deploy/compose/production.env", "production env file to validate")
	overlay := fs.String("overlay", "deploy/compose/production.yml", "production compose overlay to validate")
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

func response(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: palai response create --input <text> | palai response get <id>")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("response create", flag.ContinueOnError)
		input := fs.String("input", "", "response input text")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *input == "" {
			return errors.New("response create requires --input <text>")
		}
		return stack.CreateResponse(*input)
	case "get":
		if len(args) < 2 || args[1] == "" {
			return errors.New("usage: palai response get <id>")
		}
		return stack.GetResponse(args[1])
	default:
		return errors.New("usage: palai response create --input <text> | palai response get <id>")
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

  palai up [--env-file .env.local]
                                  ONE command: credential -> live stack -> PROVEN real round-trip.
                                  Refuses an unrecognised model selector instead of silently
                                  falling back to the deterministic fake adapter, and fails if it
                                  cannot demonstrate a real provider call.
  palai init                      generate .palai (keys, local CA, ports, config)
  palai local up                  build + start the four-service stack (retains data)
  palai local down                stop the stack, retaining data volumes
  palai local reset --confirm     stop and DELETE the data volumes
  palai local doctor [--json]     run the health checks (15: adds disk/queue/callback/runner_identity)
  palai provider add <ref>        store a provider secret (value on stdin)
  palai response create --input <text>

operability (E14 T3):
  palai config validate [--env-file <p>] [--overlay <p>] [--json]
                                  static production-posture audit (master key, edge-only surface)
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
  palai org create --display-name <n> | list | get <org_id>
  palai project create --display-name <n> | list | get <prj_id> | set-policy <prj_id> --allowed-models <a,b>
  palai apikey create --project <prj_id> [--scope <s>]... | list | get <key_id> | revoke <key_id>
  palai secret create --name <n> | list | get <name> | rotate <name>   (secret VALUE on stdin)
`)
}
