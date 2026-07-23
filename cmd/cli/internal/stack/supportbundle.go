package stack

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// supportbundle.go implements `palai support-bundle` — a single redacted tar.gz an operator can
// hand to support: the doctor verdict, `compose ps`, the compose config, the last N log lines, and
// the secret-free stack config. Nothing that reaches the tar is trusted to be secret-free: every
// part passes through the redactor (exact .palai secret VALUES + generic provider-key/bearer/env
// shapes), and supportbundle_test.go reads the produced tar back and proves zero secrets survive.

// bundlePart is one file inside the tar: its name and its raw (pre-redaction) content.
type bundlePart struct {
	name    string
	content []byte
}

// SupportBundle gathers the diagnostics for the initialised stack and writes them to outPath as a
// redacted tar.gz. It never fails the whole bundle on one missing input: a compose command that
// errors (e.g. the stack is down) records its error text as that part's content, so an operator
// diagnosing a broken stack still gets the doctor report and the config.
func SupportBundle(outPath string, tailLines int) error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	red := newRedactorFromPaths(p)
	env := cfg.composeEnv(p.home, engineImage)
	// compose runs `docker compose -p <project> -f <file> <subcommand...>`; the caller passes the
	// subcommand FIRST (e.g. compose("ps")). On error it records the output plus the error text so
	// the bundle is still produced when the stack is down.
	compose := func(sub ...string) []byte {
		full := append([]string{"compose", "-p", cfg.Project, "-f", composeFile()}, sub...)
		out, err := runCaptured(env, "docker", full...)
		if err != nil {
			return []byte(fmt.Sprintf("%s\n(command failed: %v)\n", out, err))
		}
		return []byte(out)
	}

	report := runChecks(cfg, p)
	doctorJSON, _ := json.MarshalIndent(report, "", "  ")
	stackJSON, _ := json.MarshalIndent(cfg, "", "  ")

	parts := []bundlePart{
		{"doctor.json", doctorJSON},
		{"stack-config.json", stackJSON},
		{"compose-ps.txt", compose("ps")},
		{"compose-config.yaml", compose("config")},
		{"compose-logs.txt", compose("logs", "--tail", strconv.Itoa(tailLines))},
	}

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create bundle: %w", err)
	}
	defer f.Close()
	if err := writeBundle(f, parts, red); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "support bundle written to %s (%d parts, redacted)\n", outPath, len(parts))
	return nil
}

// writeBundle redacts every part and writes them as a gzip-compressed tar to w.
func writeBundle(w io.Writer, parts []bundlePart, red *redactor) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, part := range parts {
		body := red.redact(part.content)
		hdr := &tar.Header{
			Name:    part.name,
			Mode:    0o600,
			Size:    int64(len(body)),
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar header %s: %w", part.name, err)
		}
		if _, err := tw.Write(body); err != nil {
			return fmt.Errorf("tar body %s: %w", part.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// redactor scrubs secrets from bundle content: exact known values (the .palai secret files) and
// generic secret shapes (a provider key, bearer token, or env-secret assignment that leaked into a
// log the assembler never parsed). Everything it matches becomes the marker below.
type redactor struct {
	literals []string
}

const redactedMarker = "[REDACTED]"

// wholeMatchPatterns redact their ENTIRE match — the match itself is the secret:
//   - provider API keys (OpenAI-style sk-...),
//   - HTTP bearer tokens,
//   - a PEM private-key block (multi-line).
var wholeMatchPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]+`),
	regexp.MustCompile(`(?s)-----BEGIN[A-Z ]*PRIVATE KEY-----.*?-----END[A-Z ]*PRIVATE KEY-----`),
}

// dbURLCredRe redacts the password in a database URL (postgres://user:PASS@host), keeping the
// scheme/user/host so the connection target stays a diagnostic.
var dbURLCredRe = regexp.MustCompile(`(postgres(?:ql)?://[^:@\s/]+:)[^@\s]+(@)`)

// assignmentPatterns match a `NAME<sep>VALUE` secret assignment; only the VALUE (group 3) is
// redacted, the NAME stays as a diagnostic, and a filesystem-PATH value is kept (the stack's secrets
// are file-based: `PALAI_RUNNER_CA_KEY: /palai/ca/ca.key` names a path, not a secret).
var assignmentPatterns = []*regexp.Regexp{
	// UPPER_SNAKE env vars ending KEY/PASSWORD/TOKEN/SECRET (OPENAI_API_KEY, POSTGRES_PASSWORD).
	// An uppercase name avoids nuking ordinary words that merely end in "key".
	regexp.MustCompile(`([A-Z][A-Z0-9_]*(?:KEY|PASSWORD|TOKEN|SECRET))(\s*[:=]\s*)("[^"]*"|\S+)`),
	// lowercase / quoted secret field names in logs or JSON (password=, "token": "...", api_key: ...).
	regexp.MustCompile(`(?i)("?(?:password|passwd|secret|token|api[_-]?key|access[_-]?token)"?)(\s*[:=]\s*)("[^"]*"|\S+)`),
}

// pathValueRe matches a value that is a filesystem path (a file-based secret REFERENCE, not the
// secret). Anchored and restricted to path chars so a base64 secret whose first char is `/` (e.g.
// `/9j/...==`) is NOT mistaken for a path.
var pathValueRe = regexp.MustCompile(`^(/[\w.-]+)+/?$`)

// newRedactor builds a redactor from a set of exact secret values (empty ones are ignored).
func newRedactor(literals []string) *redactor {
	var nonEmpty []string
	for _, l := range literals {
		if l != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	return &redactor{literals: nonEmpty}
}

// newRedactorFromPaths seeds the redactor with the stack's actual secret values so a log line that
// echoed one is scrubbed by exact match, not just by shape. It covers the bootstrap key and runner
// enrollment token (home root) plus EVERY file under secrets/ — pg-password, provider-*, master-key,
// and any `palai provider add <ref>` credential — so a future ref is never silently un-redacted. An
// unreadable secret (e.g. a root-owned master key) is WARNED about, so the degraded exact-match
// layer is visible rather than silently dropped.
func newRedactorFromPaths(p paths) *redactor {
	var lits []string
	seed := func(path string) {
		v, err := readTrimmed(path)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "support-bundle: WARNING: secret %s unreadable (%v) — not in the redactor's exact-match set\n", path, err)
			}
			return
		}
		lits = append(lits, v)
	}
	seed(p.apiKey)
	seed(p.runnerToken)
	if entries, err := os.ReadDir(p.secretsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				seed(filepath.Join(p.secretsDir, e.Name()))
			}
		}
	}
	return newRedactor(lits)
}

// redact replaces every known literal, then every secret-shaped match, with the marker. Literals go
// first (exact values), then the shape patterns clean up anything the assembler never knew about.
func (r *redactor) redact(b []byte) []byte {
	s := string(b)
	for _, lit := range r.literals {
		s = strings.ReplaceAll(s, lit, redactedMarker)
	}
	for _, re := range wholeMatchPatterns {
		s = re.ReplaceAllString(s, redactedMarker)
	}
	s = dbURLCredRe.ReplaceAllString(s, "${1}"+redactedMarker+"${2}")
	for _, re := range assignmentPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if val := strings.Trim(sub[3], `"`); val == "" || pathValueRe.MatchString(val) {
				return m // a path or empty value is a config reference, not a secret
			}
			return sub[1] + sub[2] + redactedMarker
		})
	}
	return []byte(s)
}

// runCaptured runs a command and returns its combined output, so a compose diagnostic's stderr is
// captured into the bundle rather than lost to the terminal.
func runCaptured(env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}
