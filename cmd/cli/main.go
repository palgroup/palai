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
	case "local":
		return local(args[1:])
	case "provider":
		return provider(args[1:])
	case "response":
		return response(args[1:])
	case "org", "project", "apikey", "secret":
		return admin.Run(args[0], args[1:], os.Stdout, os.Stdin)
	case "backup":
		return backup(args[1:])
	case "restore":
		return restore(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
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

func response(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: palai response create --input <text>")
	}
	fs := flag.NewFlagSet("response create", flag.ContinueOnError)
	input := fs.String("input", "", "response input text")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *input == "" {
		return errors.New("response create requires --input <text>")
	}
	return stack.CreateResponse(*input)
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

func usage() {
	fmt.Fprint(os.Stderr, `palai — local stack lifecycle

  palai init                      generate .palai (keys, local CA, ports, config)
  palai local up                  build + start the four-service stack (retains data)
  palai local down                stop the stack, retaining data volumes
  palai local reset --confirm     stop and DELETE the data volumes
  palai local doctor [--json]     run the health checks
  palai provider add <ref>        store a provider secret (value on stdin)
  palai response create --input <text>

installation backup/restore (whole-stack; distinct from run-level checkpoints):
  palai backup [--out <path>]              dump Postgres + object store + manifest to one archive
  palai restore --archive <path>           restore into an EMPTY target stack (refuses non-empty)
  palai restore verify --archive <path>    checksum + tenant-id + migration + run-retrieval checks

admin (thin client over the E13 APIs; base URL + key from flags, env, or .palai):
  palai org create --display-name <n> | list | get <org_id>
  palai project create --display-name <n> | list | get <prj_id> | set-policy <prj_id> --allowed-models <a,b>
  palai apikey create --project <prj_id> [--scope <s>]... | list | get <key_id> | revoke <key_id>
  palai secret create --name <n> | list | get <name> | rotate <name>   (secret VALUE on stdin)
`)
}
