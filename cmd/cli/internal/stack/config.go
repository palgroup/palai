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

	composefiles "github.com/palgroup/palai/deploy/compose"
	"github.com/palgroup/palai/packages/version"

	"github.com/palgroup/palai/packages/localca"
)

// controllerDNS is the exact DNS identity the runner gateway's server certificate carries. The compose
// runner reaches the control-plane at this name and the runner session pins it exactly (packages/runner
// requires a single matching SAN), so the certificate is signed for it and doctor probes the runner port
// under it.
//
// THE MINTER OWNS THE NAME NOW. This is an alias of localca.ControllerDNS, which is where the certificate
// that carries the name is actually signed — the control plane mints its own trust root at boot, so the
// name could no longer live in a package only the CLI can import. Keeping one spelling matters more than
// keeping it here: a second literal is how a fleet ends up trusting a certificate for a name nobody dials.
const controllerDNS = localca.ControllerDNS

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

// committedComposeFile is the repo's own compose file, relative to cwd — the path a checkout has
// always used, and the one the operator scripts and the e2e harness rely on from the repo root.
var committedComposeFile = filepath.Join("deploy", "compose", "compose.yaml")

// deployDir resolves the directory holding the deploy/compose files this run drives, in the order
// that keeps a checkout byte-for-byte the way it was:
//
//  1. PALAI_COMPOSE_FILE — the pre-existing override, still first and still absolute law (it is how
//     the e2e harness, the UAT and the production profile point the CLI at a specific file). The
//     overlay and the guard script are read from BESIDE it, which is where they live in every tree
//     that sets it.
//  2. the committed deploy/compose/compose.yaml relative to cwd — a checkout, unchanged, and
//     nothing is written anywhere.
//  3. the EMBEDDED copies materialised under ${PALAI_HOME}/compose — the case that did not exist
//     before: a `palai` binary with no source tree behind it.
func deployDir() (string, error) {
	if f := os.Getenv("PALAI_COMPOSE_FILE"); f != "" {
		return filepath.Dir(f), nil
	}
	if _, err := os.Stat(committedComposeFile); err == nil {
		return filepath.Dir(committedComposeFile), nil
	}
	return materialiseComposeFiles()
}

// resolveComposeFile returns the compose file the CLI drives. PALAI_COMPOSE_FILE is returned
// verbatim (its basename need not be compose.yaml); otherwise it is compose.yaml in deployDir.
func resolveComposeFile() (string, error) {
	if f := os.Getenv("PALAI_COMPOSE_FILE"); f != "" {
		return f, nil
	}
	dir, err := deployDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "compose.yaml"), nil
}

// materialiseComposeFiles writes the embedded deploy files to ${PALAI_HOME}/compose and returns that
// directory. They have to be REAL files on disk: `docker compose -f -` accepts stdin only once and a
// production bring-up passes two files, and config-validate reads the guard script that sits beside
// the overlay. They are rewritten on every call, so a CLI upgraded in place never keeps driving the
// previous version's compose file.
func materialiseComposeFiles() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(h, "compose")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	entries, err := composefiles.Files.ReadDir(".")
	if err != nil {
		return "", fmt.Errorf("read the embedded deploy files: %w", err)
	}
	for _, e := range entries {
		raw, err := composefiles.Files.ReadFile(e.Name())
		if err != nil {
			return "", fmt.Errorf("read the embedded %s: %w", e.Name(), err)
		}
		// The guard script is READ from here, not run (config-validate extracts its literals) — but a
		// file that looks executable and is not is its own trap, so keep the mode honest.
		mode := os.FileMode(0o600)
		if strings.HasSuffix(e.Name(), ".sh") {
			mode = 0o700
		}
		out := filepath.Join(dir, e.Name())
		if err := os.WriteFile(out, raw, mode); err != nil {
			return "", fmt.Errorf("write %s: %w", out, err)
		}
	}
	return dir, nil
}

// publishedRepoPrefix is the registry + namespace the release images are published under (it would
// read like "ghcr.io/palgroup"). It is EMPTY, and that is a measurement rather than an oversight:
// nothing has ever been published — release.yml's publish step is scripts/release/publish-dryrun.sh,
// which holds no credentials and runs every leg against a BLACKHOLED registry (plan §6 leg 2). So
// there is no reference a packaged bring-up could honestly default to, and stackImage refuses rather
// than pulling whatever happens to sit under `palai/control-plane:local` in a public registry.
//
// Setting this constant is the whole of publication day: the repository names are already the ones
// scripts/release/build.sh builds, and the tag is already this binary's release stamp.
const publishedRepoPrefix = ""

// stackImage resolves one image reference for a bring-up that has no source to build from: the
// operator's pre-existing PALAI_*_IMAGE override first, else this build's published default. name is
// the repository scripts/release/build.sh produces (control-plane, runner, reference-engine).
//
// It refuses rather than letting compose fall through to its `${PALAI_CONTROL_PLANE_IMAGE:-palai/
// control-plane:local}` default: that tag is only ever produced by a from-source build, so outside a
// checkout it can only miss — or resolve to something in a public registry that is not this release.
func stackImage(name, env string) (string, error) {
	if ref := os.Getenv(env); ref != "" {
		return ref, nil
	}
	tag, released := version.ReleaseTag()
	if publishedRepoPrefix == "" || !released {
		return "", fmt.Errorf("this build has no published image default for %s: set %s to an image reference, "+
			"or run from a checkout, which builds it (docs/operations/install.md)", name, env)
	}
	return fmt.Sprintf("%s/%s:%s", publishedRepoPrefix, name, tag), nil
}

// packagedImageEnv is composeEnv with all three image references resolved for a bring-up with no
// build context. The engine is resolved to a digest (see packagedEngineDigest) because the runner's
// lease will not accept a tag.
func packagedImageEnv(cfg Config, home string) ([]string, error) {
	cp, err := stackImage("control-plane", "PALAI_CONTROL_PLANE_IMAGE")
	if err != nil {
		return nil, err
	}
	runner, err := stackImage("runner", "PALAI_RUNNER_IMAGE")
	if err != nil {
		return nil, err
	}
	engine, err := packagedEngineDigest()
	if err != nil {
		return nil, err
	}
	return append(cfg.composeEnv(home, engine),
		"PALAI_CONTROL_PLANE_IMAGE="+cp,
		"PALAI_RUNNER_IMAGE="+runner,
	), nil
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
	masterKey   string
	// compose is the compose file every `docker compose` invocation for this stack is pointed at,
	// resolved ONCE by loadConfig (see resolveComposeFile). It rides on paths because every
	// compose-driving command already holds a paths, and because resolving it in each of them
	// would mean each deciding independently whether this is a checkout or a packaged binary.
	compose string
	// The NATIVE control plane's two files (E22 T5). A native bring-up runs the control plane as a
	// process on this machine rather than a compose service, so the two things compose would
	// otherwise hold — what is running, and what it said — have to live somewhere: the pid record
	// `palai local down` stops it by, and the log its boot refusals are written to.
	nativePID string
	nativeLog string
	// The NATIVE runner's two files (A.3 T4), and it needs its OWN pair rather than sharing the control
	// plane's: the two are separate processes with separate lifetimes — a drift repair restarts the
	// control plane alone — and one pid file holding two pids would make the reuse guard unable to say
	// which process it is refusing to kill.
	nativeRunnerPID string
	nativeRunnerLog string
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
		// The production secret-store master key (production.env.example step). Present only in the
		// production profile; the restore-verify secret canary reads it to prove restored secrets
		// decrypt under the target's key.
		masterKey:       filepath.Join(h, "secrets", "master-key"),
		nativePID:       filepath.Join(h, "control-plane.pid"),
		nativeLog:       filepath.Join(h, "control-plane.log"),
		nativeRunnerPID: filepath.Join(h, "runner.pid"),
		nativeRunnerLog: filepath.Join(h, "runner.log"),
	}, nil
}

// secretPath returns the .palai/secrets/<ref> file for a provider ref.
func (p paths) secretPath(ref string) string { return filepath.Join(p.secretsDir, ref) }

// AdminDefaults returns the base URL and the bootstrap API key of the initialised .palai stack — the last
// rung of the admin CLI's flag → env → .palai fallback chain. It errors only when .palai is absent, which
// the caller treats as fatal only when a flag/env did not already supply the value.
func AdminDefaults() (baseURL, apiKey string, err error) {
	cfg, p, err := loadConfig()
	if err != nil {
		return "", "", err
	}
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return cfg.BaseURL, "", fmt.Errorf("read api key: %w", err)
	}
	return cfg.BaseURL, key, nil
}

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
	// Every command that drives compose loads the config first, so this is the one place the
	// compose file is resolved — and, on a packaged binary, the one place the embedded deploy files
	// are written out.
	if p.compose, err = resolveComposeFile(); err != nil {
		return Config{}, p, err
	}
	return c, p, nil
}

// composeEnv is the process environment for a `docker compose` invocation: the caller's
// environment plus the ${PALAI_*} interpolation contract compose.yaml consumes. engine is
// the reference engine image reference to hand the runner — the mutable tag for a build or
// teardown, the resolved immutable sha256 digest for a boot that drives the exec-path.
// PALAI_RETENTION_STORE_FALSE_TTL (and PALAI_DISPATCH_WORKERS/PALAI_MODEL_PROVIDER) are
// inherited from os.Environ unchanged (their compose defaults hold), so an operator or the
// UAT can set them without a config field.
func (c Config) composeEnv(home, engine string) []string {
	return append(os.Environ(),
		"PALAI_HOME="+home,
		"PALAI_API_PORT="+strconv.Itoa(c.APIPort),
		"PALAI_RUNNER_PORT="+strconv.Itoa(c.RunnerPort),
		"PALAI_PG_PORT="+strconv.Itoa(c.PgPort),
		"PALAI_S3_PORT="+strconv.Itoa(c.S3Port),
		"PALAI_ENGINE_IMAGE="+engine,
		// The runner tags every engine sandbox it launches with this project so `local down`
		// can sweep exactly this stack's orphaned engines (H4).
		"PALAI_COMPOSE_PROJECT="+c.Project,
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
