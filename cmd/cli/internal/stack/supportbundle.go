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
//   - HTTP bearer tokens.
var wholeMatchPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]+`),
}

// envAssignRe matches an UPPER_SNAKE env/config assignment whose name ends in KEY/PASSWORD/TOKEN/
// SECRET (OPENAI_API_KEY, PALAI_..._TOKEN, POSTGRES_PASSWORD). Only the VALUE is redacted — the name
// stays as a diagnostic — and a filesystem-PATH value is kept, because the stack's secrets are
// file-based: a `PALAI_RUNNER_CA_KEY: /palai/ca/ca.key` line names a path, not a secret. Requiring
// an uppercase name avoids nuking ordinary words that merely end in "key".
var envAssignRe = regexp.MustCompile(`([A-Z][A-Z0-9_]*(?:KEY|PASSWORD|TOKEN|SECRET))(\s*[:=]\s*)("?[^"\s]+"?)`)

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
// echoed one is scrubbed by exact match, not just by shape. Every .palai secret file is covered —
// the bootstrap key, the Postgres password, the provider credential, the master key, and the runner
// enrollment token. Missing/empty files are skipped.
func newRedactorFromPaths(p paths) *redactor {
	var lits []string
	for _, path := range []string{
		p.apiKey,
		p.pgPassword,
		p.secretPath("provider-one"),
		filepath.Join(p.secretsDir, "master-key"),
		p.runnerToken,
	} {
		if v, err := readTrimmed(path); err == nil {
			lits = append(lits, v)
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
	s = envAssignRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := envAssignRe.FindStringSubmatch(m)
		name, sep, val := sub[1], sub[2], sub[3]
		if unquoted := strings.Trim(val, `"`); unquoted == "" || strings.HasPrefix(unquoted, "/") {
			return m // a path or empty value is a config reference, not a secret
		}
		return name + sep + redactedMarker
	})
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
