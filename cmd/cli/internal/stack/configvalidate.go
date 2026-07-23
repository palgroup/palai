package stack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// configvalidate.go implements `palai config validate` — a STATIC, stack-less audit of a
// production deploy: it reads files, never dials the running stack. It answers "will this
// production profile boot with the posture the plan requires, and is the TLS edge the only
// host-published surface?" so an operator catches a dev-default key or a re-exposed port
// BEFORE bring-up, not after. It shares the doctor Report/Check shape (and --json contract).
//
// The dev-default literals it rejects are READ from production-entrypoint.sh (parseDevDefaults),
// never re-declared here, so config-validate and the fail-closed boot guard agree by construction
// on what "dev-default" means — the set cannot fork.

// requiredEnv is the production compose interpolation contract (deploy/compose/production.env.example).
// A missing key here is an operator error the stack would only surface at bring-up.
var requiredEnv = []string{
	"PALAI_HOME",
	"PALAI_EDGE_PORT",
	"PALAI_ENGINE_IMAGE",
	"PALAI_COMPOSE_PROJECT",
	"PALAI_DISPATCH_WORKERS",
	"PALAI_MODEL_PROVIDER",
}

// optionalEnv are keys the base/overlay/observability profiles read with a compose default — present
// is fine, absent is fine; only a key in NEITHER set is flagged as an unknown (likely a typo).
var optionalEnv = map[string]bool{
	"PALAI_MODEL":                     true,
	"PALAI_RETENTION_STORE_FALSE_TTL": true,
	"PALAI_RUNNER_CERT_TTL":           true,
	"PALAI_RUNNER_CONCURRENCY":        true,
	"PALAI_METRICS_DISK_PATH":         true,
	"PALAI_PROM_PORT":                 true,
	"PALAI_GRAFANA_PORT":              true,
	"PALAI_GRAFANA_ADMIN_PASSWORD":    true,
}

// ConfigValidate runs the static posture checks and reports them like doctor. With jsonOut it
// prints the report as JSON; either way it returns a non-zero-exit error when any check is not
// green, so `palai config validate` fails an unsafe production config in a script.
func ConfigValidate(envFile, overlay string, jsonOut bool) error {
	report := validateConfig(envFile, overlay)
	if jsonOut {
		raw, err := json.Marshal(report)
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
	} else {
		for name, c := range report.Checks {
			fmt.Printf("%-18s %-8s %s\n", name, c.Status, c.Detail)
		}
	}
	if !report.OK {
		return fmt.Errorf("config validate: production posture is not safe to bring up")
	}
	if !jsonOut {
		fmt.Println("config valid")
	}
	return nil
}

func validateConfig(envFile, overlay string) Report {
	checks := map[string]Check{}

	env, envCheck := envContract(envFile)
	checks["env_contract"] = envCheck

	home := env["PALAI_HOME"]
	dd, ddErr := loadDevDefaults(overlay)

	if home == "" {
		checks["master_key"] = fail("PALAI_HOME unset in the env file — cannot locate the master key")
		checks["bootstrap_key"] = fail("PALAI_HOME unset in the env file")
		checks["cert_pair"] = fail("PALAI_HOME unset in the env file")
	} else if ddErr != nil {
		unread := fail("read dev-default literals from production-entrypoint.sh: " + ddErr.Error())
		checks["master_key"], checks["bootstrap_key"] = unread, unread
		checks["cert_pair"] = certPair(home)
	} else {
		checks["master_key"] = masterKey(home, dd.masterKeys)
		checks["bootstrap_key"] = bootstrapKey(home, dd.bootstrapKey)
		checks["cert_pair"] = certPair(home)
	}

	checks["dispatch_workers"] = dispatchWorkers(env)
	checks["edge_only_surface"] = edgeSurfaceFile(overlay)

	ok := true
	for _, c := range checks {
		if c.Status != "ok" {
			ok = false
		}
	}
	return Report{OK: ok, Checks: checks}
}

// envContract parses the env file and diagnoses missing required keys and unknown (likely
// mistyped) keys. It returns the parsed map so the other checks read the same values.
func envContract(envFile string) (map[string]string, Check) {
	env, err := parseEnvFile(envFile)
	if err != nil {
		return nil, fail("read env file: " + err.Error())
	}
	var missing, unknown []string
	for _, k := range requiredEnv {
		if _, ok := env[k]; !ok {
			missing = append(missing, k)
		}
	}
	required := map[string]bool{}
	for _, k := range requiredEnv {
		required[k] = true
	}
	for k := range env {
		if !required[k] && !optionalEnv[k] {
			unknown = append(unknown, k)
		}
	}
	if len(missing) > 0 {
		return env, fail("missing required env: " + strings.Join(missing, ", "))
	}
	if len(unknown) > 0 {
		return env, fail("unknown env (typo?): " + strings.Join(unknown, ", "))
	}
	return env, ok(fmt.Sprintf("%d required keys present, no unknown keys", len(requiredEnv)))
}

// parseEnvFile reads a KEY=VALUE env file, skipping blank lines and # comments. It does not
// interpolate — config-validate reads the literal the operator wrote.
func parseEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return env, nil
}

// masterKey checks the production master-key file: present, non-empty, not whitespace-only, and
// NOT a dev-default. It reads the file only to compare — the contents are NEVER printed.
func masterKey(home string, devDefaults []string) Check {
	path := filepath.Join(home, "secrets", "master-key")
	val, err := readTrimmed(path)
	if err != nil {
		return fail("master key file unreadable: " + err.Error())
	}
	if val == "" {
		return fail("master key file is empty or whitespace-only: " + path)
	}
	for _, dev := range devDefaults {
		if val == dev {
			return fail("master key is a dev-default — generate a real one with 'openssl rand -hex 32'")
		}
	}
	return ok("master key present and not a dev-default")
}

// bootstrapKey checks the bootstrap admin key (${PALAI_HOME}/api-key) is not the shipped
// placeholder — the "provisioning requires a REAL bootstrap key" half of the closed registration
// posture (there is no public self-registration surface by construction).
func bootstrapKey(home, devDefault string) Check {
	path := filepath.Join(home, "api-key")
	val, err := readTrimmed(path)
	if err != nil {
		return fail("bootstrap api-key file unreadable: " + err.Error())
	}
	if val == "" {
		return fail("bootstrap api-key file is empty: " + path)
	}
	if val == devDefault {
		return fail("bootstrap api-key is the shipped placeholder — mint a real one")
	}
	return ok("bootstrap api-key present and not the placeholder")
}

// certPair checks the TLS edge cert/key pair the overlay mounts is present and readable. It does
// not validate the chain — a self-minted local pair is the honest ceiling (plan §6); the operator
// swaps a real-domain cert in.
func certPair(home string) Check {
	for _, name := range []string{"server.crt", "server.key"} {
		path := filepath.Join(home, "ca", name)
		info, err := os.Stat(path)
		if err != nil {
			return fail("edge cert pair: " + err.Error())
		}
		if info.Size() == 0 {
			return fail("edge cert file is empty: " + path)
		}
	}
	return ok("edge TLS cert/key pair present and readable")
}

// dispatchWorkers checks the exec-path is on (PALAI_DISPATCH_WORKERS >= 1): a production stack
// that left it at the base default 0 would admit responses but never run them.
func dispatchWorkers(env map[string]string) Check {
	raw, ok0 := env["PALAI_DISPATCH_WORKERS"]
	if !ok0 {
		return fail("PALAI_DISPATCH_WORKERS unset (production needs >= 1)")
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fail("PALAI_DISPATCH_WORKERS not an integer: " + raw)
	}
	if n < 1 {
		return fail(fmt.Sprintf("PALAI_DISPATCH_WORKERS=%d — production needs >= 1 (queued-only otherwise)", n))
	}
	return ok(fmt.Sprintf("dispatch exec-path on (%d worker(s))", n))
}

// devDefaults holds the placeholder/zero literals the boot guard rejects, read from the guard
// script so config-validate never forks the set.
type devDefaults struct {
	masterKeys   []string
	bootstrapKey string
}

// loadDevDefaults reads the guard script that sits beside the overlay and extracts its literals.
func loadDevDefaults(overlay string) (devDefaults, error) {
	script := filepath.Join(filepath.Dir(overlay), "production-entrypoint.sh")
	raw, err := os.ReadFile(script)
	if err != nil {
		return devDefaults{}, err
	}
	return parseDevDefaults(string(raw))
}

var devAssignRe = regexp.MustCompile(`(?m)^\s*(DEV_[A-Z_]+)="([^"]*)"`)

// parseDevDefaults extracts the DEV_*="..." assignments from production-entrypoint.sh: values of
// vars whose name mentions MASTER back the master-key rejection, the BOOTSTRAP one the bootstrap
// rejection. Keyed on the name substring so it survives an added dev-default without a code change.
func parseDevDefaults(script string) (devDefaults, error) {
	var dd devDefaults
	for _, m := range devAssignRe.FindAllStringSubmatch(script, -1) {
		name, val := m[1], m[2]
		switch {
		case strings.Contains(name, "MASTER"):
			dd.masterKeys = append(dd.masterKeys, val)
		case strings.Contains(name, "BOOTSTRAP"):
			dd.bootstrapKey = val
		}
	}
	if len(dd.masterKeys) == 0 || dd.bootstrapKey == "" {
		return dd, fmt.Errorf("no DEV_MASTER/DEV_BOOTSTRAP literals found in the guard script")
	}
	return dd, nil
}

// edgeSurfaceFile reads the overlay and runs edgeSurfaceCheck. It is the NIT-8 machine-check: the
// TLS edge is the ONLY host-published surface and the Caddyfile proxies only /v1/*.
func edgeSurfaceFile(overlay string) Check {
	raw, err := os.ReadFile(overlay)
	if err != nil {
		return fail("read production overlay: " + err.Error())
	}
	return edgeSurfaceCheck(string(raw))
}

// internalServices are the base services that MUST reset their host ports under the production
// overlay — none may be reachable from the host, only the edge.
var internalServices = []string{"postgres", "object-store", "control-plane", "runner"}

// edgeSurfaceCheck asserts, statically, that the production overlay publishes ONLY the edge to the
// host and that its Caddyfile reverse_proxy is path-matched to /v1/* (so /metrics and /healthz —
// the unauthenticated internal probes on the same top mux — are not reachable through the edge).
func edgeSurfaceCheck(overlayContent string) Check {
	var doc struct {
		Services map[string]struct {
			Ports yaml.Node `yaml:"ports"`
		} `yaml:"services"`
		Configs map[string]struct {
			Content string `yaml:"content"`
		} `yaml:"configs"`
	}
	if err := yaml.Unmarshal([]byte(overlayContent), &doc); err != nil {
		return fail("parse overlay YAML: " + err.Error())
	}

	// Every service other than the edge must publish NO host port; the edge must publish one.
	for name, svc := range doc.Services {
		published := len(svc.Ports.Content) > 0
		if name == "edge" {
			if !published {
				return fail("edge service publishes no host port — nothing would be reachable")
			}
			continue
		}
		if published {
			return fail(fmt.Sprintf("service %q publishes a host port — only the edge may (re-exposed surface)", name))
		}
	}
	// Each internal service must be present in the overlay with its base ports reset — an omitted
	// service would keep the base compose's published ports.
	for _, name := range internalServices {
		if _, present := doc.Services[name]; !present {
			return fail(fmt.Sprintf("overlay does not reset host ports for %q — its base ports would stay published", name))
		}
	}

	caddy, present := doc.Configs["edge_caddyfile"]
	if !present || strings.TrimSpace(caddy.Content) == "" {
		return fail("overlay has no edge_caddyfile config — cannot verify the /v1/* path match")
	}
	if err := assertV1Only(caddy.Content); err != nil {
		return fail(err.Error())
	}
	return ok("edge is the only host-published surface; Caddyfile proxies only /v1/* (/metrics + /healthz not edge-reachable)")
}

// assertV1Only verifies every reverse_proxy directive in the Caddyfile is path-matched under
// /v1/. A directive with no path matcher (the token after reverse_proxy is an upstream, not a
// path) catches everything including /metrics, so it is rejected.
func assertV1Only(caddyfile string) error {
	sawProxy := false
	for _, line := range strings.Split(caddyfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "reverse_proxy" {
			continue
		}
		sawProxy = true
		matcher := fields[1]
		if !strings.HasPrefix(matcher, "/v1/") {
			return fmt.Errorf("edge reverse_proxy is not /v1/-matched (%q) — it would expose /metrics or /healthz", matcher)
		}
	}
	if !sawProxy {
		return fmt.Errorf("edge Caddyfile has no reverse_proxy directive")
	}
	return nil
}
