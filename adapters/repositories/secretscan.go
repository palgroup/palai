package repositories

import "regexp"

// SecretFinding is one likely-committed-secret hit over content entering a changeset (spec §30.4
// committed-secret detection). Rule names the matched shape; it deliberately carries NO captured
// secret value, so a finding is safe to persist, display, and put in an evidence manifest.
type SecretFinding struct {
	Rule string
}

// committedSecretPatterns are the secret shapes flagged in content entering a changeset (spec §30.4).
// They mirror the shell-output redaction set (adapters/sandboxes/oci/workspace/exec.go secretPatterns)
// plus a private-key header — the SAME shapes, applied here as DETECTION rather than masking. This is
// the committed-secret scanner the preparation path deferred to the changeset (prepare.go step 7-8
// note), and the seam tests/security exercises.
//
// ponytail: a focused, shape-based set (provider keys, GitHub tokens/PATs, private keys, bearer
// tokens) — the shapes the credential-hygiene grep already hunts. Extend it for a new shape rather
// than reaching for a full-entropy scanner; entropy scanning is the upgrade path if false-negatives
// on random-looking secrets ever matter.
var committedSecretPatterns = []struct {
	rule string
	re   *regexp.Regexp
}{
	{"provider_api_key", regexp.MustCompile(`sk-[A-Za-z0-9._-]{6,}`)},
	{"github_token", regexp.MustCompile(`gh[posu]_[A-Za-z0-9]{20,}`)},
	{"github_pat", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"bearer_token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{8,}`)},
}

// ScanSecrets returns the distinct secret-shape rules that match content (spec §30.4). It reports the
// matched RULE names, never the secret value, so the result is safe to store and surface. Each rule
// appears at most once even on multiple matches; order is the pattern order.
func ScanSecrets(content string) []SecretFinding {
	var out []SecretFinding
	for _, p := range committedSecretPatterns {
		if p.re.MatchString(content) {
			out = append(out, SecretFinding{Rule: p.rule})
		}
	}
	return out
}
