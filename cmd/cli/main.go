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
	case "config":
		return config(args[1:])
	case "support-bundle":
		return supportBundle(args[1:])
	case "org", "project", "apikey", "secret":
		return admin.Run(args[0], args[1:], os.Stdout, os.Stdin)
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

func usage() {
	fmt.Fprint(os.Stderr, `palai — local stack lifecycle

  palai init                      generate .palai (keys, local CA, ports, config)
  palai local up                  build + start the four-service stack (retains data)
  palai local down                stop the stack, retaining data volumes
  palai local reset --confirm     stop and DELETE the data volumes
  palai local doctor [--json]     run the health checks (14: adds disk/queue/callback)
  palai provider add <ref>        store a provider secret (value on stdin)
  palai response create --input <text>

operability (E14 T3):
  palai config validate [--env-file <p>] [--overlay <p>] [--json]
                                  static production-posture audit (master key, edge-only surface)
  palai support-bundle [--out <p>] [--tail <n>]
                                  redacted diagnostics tar.gz (doctor + compose ps/config/logs)

admin (thin client over the E13 APIs; base URL + key from flags, env, or .palai):
  palai org create --display-name <n> | list | get <org_id>
  palai project create --display-name <n> | list | get <prj_id> | set-policy <prj_id> --allowed-models <a,b>
  palai apikey create --project <prj_id> [--scope <s>]... | list | get <key_id> | revoke <key_id>
  palai secret create --name <n> | list | get <name> | rotate <name>   (secret VALUE on stdin)
`)
}
