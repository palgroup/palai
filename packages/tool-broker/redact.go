package toolbroker

import (
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
