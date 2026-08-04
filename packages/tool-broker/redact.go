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
}

// RedactSecrets masks secret-shaped tokens in captured shell output. The command's own output is
// untrusted; a runner does not rely on it having redacted itself.
//
// It lives beside ShellResult rather than inside one runner because redaction is a property of the
// RESULT, not of the container that produced it: the OCI executor and the host executor call the
// same function, so a new secret shape added for one posture cannot be missing from the other.
func RedactSecrets(s string) string {
	for _, pattern := range secretPatterns {
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
