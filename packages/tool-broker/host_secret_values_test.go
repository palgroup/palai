package toolbroker

import (
	"strings"
	"testing"
)

// TestAControlPlaneSecretIsMaskedInToolOutput reproduces the leak measured on a live native stack,
// 2026-08-04, and it is worth stating exactly because the obvious fixes do not work.
//
// The shell tool's child environment is an allow-list of three names, so an agent cannot INHERIT a
// secret. It runs as the same uid as the control plane, so it can READ one:
//
//	ps -E -p <control-plane pid>   → 62 variables, values included
//
// `os.Unsetenv` does NOT help — measured, not assumed: macOS answers `ps` from the kernel's copy of
// the process's initial environment, so a value that ever entered it stays visible for the life of
// the process. The defence therefore has to act on the VALUE wherever it surfaces.
func TestAControlPlaneSecretIsMaskedInToolOutput(t *testing.T) {
	t.Setenv("PALAI_SECRET_PROVIDER_ONE", "sk-proj-fakevalue-for-this-test-only")
	captured := "PALAI_SECRET_PROVIDER_ONE=sk-proj-fakevalue-for-this-test-only OTHER=1"

	masked := RedactValues(captured, HostSecretValues())
	if strings.Contains(masked, "fakevalue-for-this-test-only") {
		t.Errorf("the control plane's provider key survived tool output: %q", masked)
	}
}

// TestAShapeTheBlocklistCannotSeeIsStillMasked is the reason this exists at all. RedactSecrets is a
// blocklist of four regexps (sk-, bearer, gh?_, github_pat_) and it cannot recognise an operator's
// database password, a Slack token or a PEM block. A NAME test can, because the operator already
// told us what the variable is by calling it a secret.
func TestAShapeTheBlocklistCannotSeeIsStillMasked(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		// Assembled rather than a literal: GitHub push protection blocks a real-shaped Slack token,
		// which is itself evidence the shape is distinctive and that our blocklist misses it.
		{"SLACK_BOT_TOKEN", "xoxb-" + "11111111111-22222222222-abcdefghijklmnop"},
		{"PALAI_DB_PASSWORD", "correct-horse-battery-staple"},
		{"POSTGRES_DSN", "postgres://palai:hunter2@127.0.0.1:5432/palai"},
		{"GITHUB_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if RedactSecrets(tc.value) != tc.value {
				t.Skip("the shape blocklist already covers this one; nothing for the name test to prove")
			}
			t.Setenv(tc.name, tc.value)
			if masked := RedactValues(tc.value, HostSecretValues()); strings.Contains(masked, tc.value) {
				t.Errorf("%s survived redaction: the blocklist cannot see this shape and the name test did not fire", tc.name)
			}
		})
	}
}

// TestAPathIsNotMasked — a secret-named variable often holds a POINTER rather than a secret
// (PALAI_SECRET_MASTER_KEY_FILE, PALAI_RUNNER_CA_KEY). Masking a filesystem path would replace it
// everywhere it appears and make error messages unreadable, which is how an operator learns to
// distrust the output they are supposed to be reading.
func TestAPathIsNotMasked(t *testing.T) {
	t.Setenv("PALAI_SECRET_MASTER_KEY_FILE", "/etc/palai/master.key")
	for _, v := range HostSecretValues() {
		if strings.HasPrefix(v, "/") {
			t.Errorf("a path was offered for masking: %q", v)
		}
	}
}

// TestAnOrdinaryVariableIsNotMasked — over-masking is its own defect. If PATH or a workspace root
// were redacted, every command's output would be mutilated and the feature would be turned off.
func TestAnOrdinaryVariableIsNotMasked(t *testing.T) {
	t.Setenv("PALAI_WORKSPACE_ROOT", "relative-looking-value")
	t.Setenv("LANG", "en_US.UTF-8")
	for _, v := range HostSecretValues() {
		if v == "relative-looking-value" || v == "en_US.UTF-8" {
			t.Errorf("a non-secret variable was offered for masking: %q", v)
		}
	}
}

// TestTheNameTestMatchesTheNamesThisTreeActuallyUses pins the pattern against real variable names
// from this deployment, so a rename that quietly drops one out of scope fails here rather than in
// production.
func TestTheNameTestMatchesTheNamesThisTreeActuallyUses(t *testing.T) {
	for _, name := range []string{
		"PALAI_SECRET_PROVIDER_ONE", "PALAI_SECRET_PROVIDER_TWO", "PALAI_RUNNER_CA_KEY",
		"SLACK_SIGNING_SECRET", "GITHUB_APP_PRIVATE_KEY", "PALAI_DB_PASSWORD",
	} {
		if !sensitiveHostEnvNames.MatchString(name) {
			t.Errorf("%q is not recognised as secret-named; its value would reach tool output verbatim", name)
		}
	}
	for _, name := range []string{"PATH", "LANG", "PALAI_WORKSPACE_ROOT", "PALAI_LISTEN_ADDR"} {
		if sensitiveHostEnvNames.MatchString(name) {
			t.Errorf("%q is treated as secret-named; masking it would mutilate ordinary output", name)
		}
	}
}

// TestACredentialInsideAURLIsMasked covers the one secret-bearing variable that CANNOT leave the
// environment: PALAI_DATABASE_URL. The process needs it to dial Postgres, so unlike a provider key it
// cannot be moved into the encrypted store — redaction has to carry this case alone.
//
// It defeats both other defences on its own. Its NAME says nothing about secrets, so the name test in
// HostSecretValues does not select it; and its SHAPE matched none of the four token regexps. An agent
// reading `ps` therefore got a working database password in clear text.
func TestACredentialInsideAURLIsMasked(t *testing.T) {
	masked := RedactSecrets("postgres://palai:hunter2@127.0.0.1:5432/palai?sslmode=disable")
	if strings.Contains(masked, "hunter2") {
		t.Errorf("the database password survived redaction: %q", masked)
	}
	// The scheme and host must SURVIVE. An operator debugging a connection needs to see where it
	// points, and a redactor that eats the whole URL gets turned off by whoever is on call.
	if !strings.Contains(masked, "postgres://") || !strings.Contains(masked, "127.0.0.1:5432") {
		t.Errorf("redaction destroyed the part an operator needs: %q", masked)
	}
}

// TestAnOrdinaryURLIsUntouched — over-masking is the failure mode that gets a redactor disabled. A URL
// with no credential in it must come back byte-identical.
func TestAnOrdinaryURLIsUntouched(t *testing.T) {
	for _, u := range []string{
		"https://api.example.com/v1/messages",
		"http://127.0.0.1:60351/healthz",
		"postgres://127.0.0.1:5432/palai?sslmode=disable",
	} {
		if got := RedactSecrets(u); got != u {
			t.Errorf("a credential-free URL was mangled: %q → %q", u, got)
		}
	}
}
