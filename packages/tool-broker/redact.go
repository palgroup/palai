package toolbroker

import (
	"os"
	"regexp"
	"strings"
)

// secretPatterns are the secret shapes masked in shell output before it is displayed or returned
// (spec §28.8 secret redaction). ponytail: a focused set (provider keys, bearer tokens, GitHub
// tokens) mirroring the supervisor's stderr redaction; extend it for a new shape rather than
// reaching for a full-entropy scanner.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9._-]{6,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{8,}`),
	regexp.MustCompile(`gh[posu]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	// A credential embedded in a URL: postgres://user:pass@host, redis://…, https://user:token@…
	// Added 2026-08-04 because PALAI_DATABASE_URL is the one secret-bearing variable that CANNOT leave
	// the environment — the process needs it to dial — so it is the case redaction has to carry alone.
	// Its NAME defeats a name test (nothing in it says secret), and its shape defeated this list until
	// now. Only the credential is masked; the scheme and host stay readable, because an operator
	// debugging a connection needs to see where it points.
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/:@]+:[^\s/@]+@`),
}

// RedactSecrets masks secret-shaped tokens in captured shell output. The command's own output is
// untrusted; a runner does not rely on it having redacted itself.
//
// It lives beside ShellResult rather than inside one runner because redaction is a property of the
// RESULT, not of the container that produced it: the OCI executor and the host executor call the
// same function, so a new secret shape added for one posture cannot be missing from the other.
func RedactSecrets(s string) string {
	for _, pattern := range secretPatterns {
		// A pattern with a capture group keeps what it captured and masks the rest: the URL-credential
		// pattern preserves the scheme so `postgres://***@host` still tells an operator what it is.
		if pattern.NumSubexp() > 0 {
			s = pattern.ReplaceAllString(s, "${1}***@")
			continue
		}
		s = pattern.ReplaceAllString(s, "***")
	}
	return s
}

// RedactValues is RedactSecrets' VALUE-based sibling (E25 T3): it masks the attempt's OWN environment
// values by literal substring match. RedactSecrets is shape-based and cannot see an environment value —
// a Jira API token, a database password or an internal base URL matches none of its four patterns — so
// without this an `env`, a `set -x` trace or an error message echoing a credential would land verbatim
// in the tool ledger, in the run's events, and on an operator's screen.
//
// It lives beside RedactSecrets and BOTH executors call both, for the reason stated above: redaction is
// a property of the RESULT, not of the container.
//
// HONEST CEILING, AND IT IS THE WHOLE OF WHAT THIS FUNCTION CLAIMS. This is a literal substring match.
// An agent that base64-encodes a value, prints it one character per line, reverses it, or splits it
// across two commands defeats it completely, and nothing here can prevent that — giving an agent a
// secret IS the agent having that secret. What it is real against is ACCIDENTAL leakage: a build log
// that echoes its own environment, a curl that prints its Authorization header, a stack trace carrying
// a connection string. That is the failure that happens by default; the hostile one does not, and is
// out of reach of any redactor.
//
// Values shorter than 4 bytes are skipped: masking every "1" or "ok" in a build log would destroy the
// output while protecting nothing that could be called a credential.
func RedactValues(s string, values []string) string {
	for _, v := range values {
		if len(v) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}

// sensitiveHostEnvNames matches the NAME of a host environment variable whose VALUE must never
// survive in tool output. It is deliberately a name test rather than a value test: the value is
// whatever the operator's deployment happens to carry, and no shape scanner recognises a Postgres
// password or a base64 blob.
var sensitiveHostEnvNames = regexp.MustCompile(`(?i)(SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|_KEY$|APIKEY|API_KEY|DSN|CONNECTION_STRING)`)

// HostSecretValues returns the VALUES of this process's own secret-named environment variables, for
// RedactValues.
//
// WHY THIS EXISTS, measured on a live native stack 2026-08-04. The shell tool's child environment is
// an allow-list of three names, so an agent cannot inherit a secret — that hole was already closed.
// It can still READ one, because it runs as the same uid as the control plane:
//
//	ps -E -p <control-plane pid>   → 62 variables, values included
//
// and `os.Unsetenv` does NOT help, which was measured rather than assumed: macOS serves `ps` from the
// kernel's copy of the process's initial environment (KERN_PROCARGS2), so a value that ever entered
// the environment stays visible for the life of the process. The same reachability covers reading
// `.palai/api-key` off disk — correct 0600 permissions are no barrier to the same uid.
//
// So the defence has to act on the VALUE wherever it appears, which is what this feeds. It catches
// every shape secretPatterns cannot: Slack `xoxb-`, a PEM block, a `postgres://user:pass@` DSN, an
// operator's own database password.
//
// TWO CEILINGS, both real, neither hidden. (1) This redacts OUTPUT. An agent that reads a value and
// writes it to a file, or sends it over the network, is not stopped — that needs the credential off
// the machine entirely (a gateway) and the agent under a different uid. (2) A value shorter than four
// bytes is skipped by RedactValues, so a trivially short secret is not masked; that bound is the
// existing one and is deliberate, since masking every "1" in the output would destroy it.
func HostSecretValues() []string {
	var out []string
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if !found || value == "" || !sensitiveHostEnvNames.MatchString(name) {
			continue
		}
		// A PATH-like value that merely lives under a secret-named variable would mask harmless text
		// everywhere it appears. Only values that are plausibly opaque are worth masking; a filesystem
		// path is a POINTER to a secret, not the secret, and masking it makes error messages unreadable.
		if strings.HasPrefix(value, "/") {
			continue
		}
		out = append(out, value)
	}
	return out
}

// HostSecretFileValues returns the CONTENTS of the credential files this process's environment points
// at, for RedactValues.
//
// IT CLOSES THE HALF HostSecretValues DELIBERATELY LEAVES OPEN. That function skips any value starting
// with "/" because a path is a POINTER to a secret and masking it would mutilate every error message
// that names a file. But the thing it points AT is the secret, and on this posture the agent runs as
// the same uid as the control plane — measured 2026-08-05, `key_local` carries {system, provision,
// approve} with no expiry and lives in a 0600 file that same uid reads freely. `cat` on it produced
// clean output until this existed.
//
// The environment names them all: PALAI_BOOTSTRAP_API_KEY_FILE, PALAI_SECRET_MASTER_KEY_FILE,
// PALAI_GITHUB_APP_PRIVATE_KEY_FILE, PALAI_ENROLLMENT_TOKEN_FILE, PALAI_RUNNER_CA_KEY.
//
// BOUNDED ON PURPOSE. Only files under 64 KiB are read: a credential is small, and a redactor that
// slurps whatever an operator pointed a variable at is a denial-of-service against its own tool call.
// Read errors are silent — a missing or unreadable file masks nothing, which is the same posture as a
// missing variable, and a redactor that failed loudly would turn every tool call into an incident.
func HostSecretFileValues() []string {
	var out []string
	for _, entry := range os.Environ() {
		name, path, found := strings.Cut(entry, "=")
		if !found || !strings.HasPrefix(path, "/") || !sensitiveHostEnvNames.MatchString(name) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 64*1024 {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// A trailing newline is what `cat` prints and what a file written by `printf` usually lacks, so the
		// stored form and the echoed form differ by one byte. Trim, or the mask misses the common case.
		if value := strings.TrimSpace(string(raw)); len(value) >= 4 {
			out = append(out, value)
		}
	}
	return out
}

// RedactAnyValues walks a decoded structure and masks `values` in every string it contains, returning a
// new value; the input is not mutated.
//
// IT EXISTS BECAUSE A TOOL RESULT IS NOT A STRING. RedactSecrets and RedactValues take text, which fits
// shell output exactly and fits nothing else: an MCP tool answers with a decoded map, and applying a
// string redactor to it means either skipping the nested values or re-serialising to JSON first. The
// second is the trap — encoding/json escapes `<`, `>` and `&`, so a value containing one would travel as
// an escape sequence and a substring match would never fire. This tree has paid for that twice. So the
// walk happens on the DECODED structure, where a string is the string the model will read.
//
// Map KEYS are walked too. A server that answers `{"<token>": "ok"}` puts the secret in the key, and a
// redactor that only visited values would hand it back intact.
func RedactAnyValues(v any, values []string) any {
	if len(values) == 0 {
		return v
	}
	switch t := v.(type) {
	case string:
		return RedactValues(t, values)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[RedactValues(k, values)] = RedactAnyValues(item, values)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = RedactAnyValues(item, values)
		}
		return out
	default:
		// Numbers, bools and nil carry no secret a value-match could find, and JSON decoding produces
		// nothing else. A type this does not know is returned unchanged rather than stringified — a
		// redactor that rewrites what it does not understand corrupts results to no benefit.
		return v
	}
}

// RedactAnySecrets applies the SHAPE blocklist to every string in a decoded structure, the way
// RedactSecrets does for text. Same walk, same reason.
func RedactAnySecrets(v any) any {
	switch t := v.(type) {
	case string:
		return RedactSecrets(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[RedactSecrets(k)] = RedactAnySecrets(item)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = RedactAnySecrets(item)
		}
		return out
	default:
		return v
	}
}
