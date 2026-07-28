package toolbroker

import "regexp"

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
