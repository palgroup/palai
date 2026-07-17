// Package stack implements the `palai` local-stack lifecycle: it initialises the .palai
// data layout, drives the four-service Docker Compose distribution up and down, and runs
// the doctor health surface. It shells out to `docker compose` (no Docker SDK dependency)
// and speaks the public API and the durable spine over the ports `init` minted, so the
// same binary an operator runs is what the e2e proof drives.
package stack

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// controllerDNS is the exact DNS identity the runner gateway's server certificate
// carries. The compose runner reaches the control-plane at this name and the runner
// session pins it exactly (packages/runner requires a single matching SAN), so `init`
// signs the server certificate for it and doctor probes the runner port under it.
const controllerDNS = "control-plane"

// engineImage is the stable local tag `local up` builds the reference engine under and
// passes to the runner as PALAI_ENGINE_IMAGE. It is locally-built (no release digest —
// that pinning is E18), so doctor labels it accordingly.
const engineImage = "palai/reference-engine:local"

// Config is the .palai/config.json contract the CLI writes at init and the e2e harness
// reads: the compose project identity, the data dir, the published host ports, and the
// surfaces derived from them. The field names are the JSON the harness decodes.
type Config struct {
	Project       string `json:"project"`
	DataDir       string `json:"data_dir"`
	APIPort       int    `json:"api_port"`
	RunnerPort    int    `json:"runner_port"`
	PgPort        int    `json:"pg_port"`
	S3Port        int    `json:"s3_port"`
	BaseURL       string `json:"base_url"`
	ControllerDNS string `json:"controller_dns"`
}

// home resolves the .palai data dir: PALAI_HOME when set (the e2e harness points it at an
// isolated temp dir), else ./.palai. It is returned absolute so compose interpolation and
// bind-mount sources never depend on the caller's cwd.
func home() (string, error) {
	h := os.Getenv("PALAI_HOME")
	if h == "" {
		h = ".palai"
	}
	return filepath.Abs(h)
}

// composeFile resolves the compose file path: PALAI_COMPOSE_FILE when set, else the
// committed deploy/compose/compose.yaml relative to cwd (the operator scripts and the
// harness both run from the repo root).
func composeFile() string {
	if f := os.Getenv("PALAI_COMPOSE_FILE"); f != "" {
		return f
	}
	return filepath.Join("deploy", "compose", "compose.yaml")
}

// paths holds the resolved .palai file locations for one stack.
type paths struct {
	home        string
	config      string
	apiKey      string
	runnerToken string
	caDir       string
	caCert      string
	caKey       string
	serverCert  string
	serverKey   string
	secretsDir  string
	pgPassword  string
}

func resolvePaths() (paths, error) {
	h, err := home()
	if err != nil {
		return paths{}, err
	}
	return paths{
		home:        h,
		config:      filepath.Join(h, "config.json"),
		apiKey:      filepath.Join(h, "api-key"),
		runnerToken: filepath.Join(h, "runner-token"),
		caDir:       filepath.Join(h, "ca"),
		caCert:      filepath.Join(h, "ca", "ca.crt"),
		caKey:       filepath.Join(h, "ca", "ca.key"),
		serverCert:  filepath.Join(h, "ca", "server.crt"),
		serverKey:   filepath.Join(h, "ca", "server.key"),
		secretsDir:  filepath.Join(h, "secrets"),
		pgPassword:  filepath.Join(h, "secrets", "pg-password"),
	}, nil
}

// secretPath returns the .palai/secrets/<ref> file for a provider ref.
func (p paths) secretPath(ref string) string { return filepath.Join(p.secretsDir, ref) }

// loadConfig reads .palai/config.json. A missing file means the stack was never
// initialised, which the callers surface as an actionable error.
func loadConfig() (Config, paths, error) {
	p, err := resolvePaths()
	if err != nil {
		return Config{}, paths{}, err
	}
	raw, err := os.ReadFile(p.config)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, p, fmt.Errorf("not initialised: run `palai init` first (%s)", p.config)
		}
		return Config{}, p, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, p, fmt.Errorf("decode config.json: %w", err)
	}
	return c, p, nil
}

// composeEnv is the process environment for a `docker compose` invocation: the caller's
// environment plus the ${PALAI_*} interpolation contract compose.yaml consumes.
// PALAI_RETENTION_STORE_FALSE_TTL is inherited from os.Environ unchanged (its compose
// default is empty), so an operator can enable reaping without a config field.
func (c Config) composeEnv(home string) []string {
	return append(os.Environ(),
		"PALAI_HOME="+home,
		"PALAI_API_PORT="+strconv.Itoa(c.APIPort),
		"PALAI_RUNNER_PORT="+strconv.Itoa(c.RunnerPort),
		"PALAI_PG_PORT="+strconv.Itoa(c.PgPort),
		"PALAI_S3_PORT="+strconv.Itoa(c.S3Port),
		"PALAI_ENGINE_IMAGE="+engineImage,
	)
}

// databaseURL is the host-side Postgres URL doctor uses for its clock and migration
// probes, built from the published port and the minted pg password.
func (c Config) databaseURL(password string) string {
	return fmt.Sprintf("postgres://palai:%s@127.0.0.1:%d/palai?sslmode=disable", password, c.PgPort)
}

// freePorts reserves n distinct loopback TCP ports by holding a listener on each until
// all are chosen, then releasing them. A port can in principle be taken in the race
// window before compose binds it; the isolated-port design matches the sse/responses
// tiers and is the pragmatic local-dev choice.
func freePorts(n int) ([]int, error) {
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("reserve port: %w", err)
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// readTrimmed reads a .palai file and trims surrounding whitespace — the minted key,
// token, and password files carry no trailing newline, but trimming keeps a hand-edited
// file working.
func readTrimmed(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
