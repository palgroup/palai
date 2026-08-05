package stack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// E22 T4: the GitHub App an APPROVED publication publishes through.
//
// THE FAILURE THIS FILE REFUSES is the most expensive shape of E21 T2's silent skip, because a human is
// inside it. repositoryPublisherFromEnv returned nil when any of its three variables was missing, and a nil
// publisher made the approval pump a no-op. Every surface then reported success: the model gets
// pending_approval, the approver presses Approve, the Slack message is repaired to "Approved: push agent/…",
// the publication row says approved. And the branch is never pushed. Not failed — never attempted, with no
// error, no log and no retry anywhere.
//
// THE CONTROL-PLANE HALF OF THAT WAS FIXED (main.repositoryPublisher builds the connection_ref path
// independently of the App, and a publication with no credential path is refused rather than dropped), and
// these tests are unchanged by it because they assert a DIFFERENT thing: what `palai up` tells the operator
// before the stack is even built, and where the App private key's bytes go. The bring-up warning still
// earns its line for the binding `palai up` itself creates — PALAI_GIT_CLONE_URL yields a binding with NO
// connection_ref, which is precisely the one that cannot publish without an App.
//
// So the three variables are configuration, and their ABSENCE is a warning an operator can read.

// appEnv is a fully configured §0.2 environment pointing at a key file this test wrote.
func appEnv(t *testing.T, keyPath string) map[string]string {
	t.Helper()
	return map[string]string{
		"PALAI_GIT_CLONE_URL":               "https://github.com/acme/widgets.git",
		"PALAI_GIT_BASE_BRANCH":             "dev",
		"PALAI_GIT_REPO":                    "acme/widgets",
		"PALAI_GITHUB_APP_ID":               "123456",
		"PALAI_GITHUB_APP_INSTALLATION_ID":  "7891011",
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE": keyPath,
	}
}

// testAppKey is a stand-in for the App's PEM. It is not a key and cannot sign anything; what the tests
// below assert about it is where its BYTES end up, which is a property of the plumbing, not of the key.
const testAppKey = "-----BEGIN RSA PRIVATE KEY-----\nNOT-A-REAL-KEY-e22t4\n-----END RSA PRIVATE KEY-----\n"

// clearAppEnv makes the process environment neutral AND restores it after the test — t.Setenv's cleanup is
// what lets these tests read back what applyGitHubAppEnv exported without leaking into their neighbours.
func clearAppEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"PALAI_GITHUB_APP_ID", "PALAI_GITHUB_APP_INSTALLATION_ID",
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE", "PALAI_GITHUB_REPO"} {
		t.Setenv(name, "")
	}
}

// TestApplyGitHubAppEnvIsSilentWhenNothingIsConfigured replaces a test that asserted a warning HERE.
//
// That warning ("a repository is bound but no GitHub App is configured") was gated on
// PALAI_GIT_CLONE_URL, and on 2026-08-05 bindings became remote-only and that variable lost its last
// reader. The condition became unsatisfiable — the warning could never fire again while still reading,
// to anyone scanning this file, as coverage of the failure it named.
//
// The question is still worth asking and now runs where the answer LIVES: missingPublisherNotice reads
// the actual binding rows after the stack is up, and refuses only the bindings that carry no
// connection_ref either. Its tests are in up_repository_test.go.
//
// What is asserted here is what remains true of this function: with nothing configured it exports
// nothing and says nothing.
func TestApplyGitHubAppEnvIsSilentWhenNothingIsConfigured(t *testing.T) {
	clearAppEnv(t)
	warns := applyGitHubAppEnv(tempPaths(t), envGetter(map[string]string{
		// Set deliberately: these no longer bind anything, and this function must not have grown an
		// opinion about them again.
		"PALAI_GIT_CLONE_URL":   "https://github.com/acme/widgets.git",
		"PALAI_GIT_BASE_BRANCH": "dev",
	}))
	if len(warns) != 0 {
		t.Fatalf("applyGitHubAppEnv warned about a repository it no longer knows anything about: %v", warns)
	}
}

// ...and a stack that bound NO repository is NOT warned. It never asked to publish anything, and a warning
// on every bring-up is the crying-wolf `palai up` deliberately removed from the orphan sweep.
func TestAStackThatBoundNoRepositoryIsNotWarnedAboutAPublisher(t *testing.T) {
	clearAppEnv(t)
	if warns := applyGitHubAppEnv(tempPaths(t), envGetter(nil)); len(warns) != 0 {
		t.Fatalf("a stack with no repository was warned about a GitHub App it never needed: %v", warns)
	}
}

// TestAHalfConfiguredGitHubAppIsRefusedByName: two of three is the state an operator reaches by editing
// .env.local and being interrupted. It must not read as "configured" and must not read as "unconfigured" —
// it is the one case where the operator believes they finished.
func TestAHalfConfiguredGitHubAppIsRefusedByName(t *testing.T) {
	key := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(key, []byte(testAppKey), 0o600); err != nil {
		t.Fatal(err)
	}
	full := appEnv(t, key)
	for _, missing := range []string{"PALAI_GITHUB_APP_ID", "PALAI_GITHUB_APP_INSTALLATION_ID",
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE"} {
		t.Run(missing, func(t *testing.T) {
			clearAppEnv(t)
			env := map[string]string{}
			for k, v := range full {
				env[k] = v
			}
			env[missing] = ""
			p := tempPaths(t)
			warns := applyGitHubAppEnv(p, envGetter(env))
			if len(warns) != 1 || !strings.Contains(warns[0], missing) {
				t.Fatalf("a half-configured App (%s unset) warned %v, want one warning naming it", missing, warns)
			}
			// Nothing was exported and nothing was staged: half a configuration configures nothing.
			if got := os.Getenv("PALAI_GITHUB_APP_ID"); got != "" {
				t.Fatalf("a half-configured App still exported PALAI_GITHUB_APP_ID=%q", got)
			}
			if _, err := os.Stat(p.secretPath(gitHubAppKeySlot)); err == nil {
				t.Fatal("a half-configured App staged the private key anyway: a credential must not be copied for " +
					"a publisher that will not exist")
			}
		})
	}
}

// TestTheGitHubAppKeyRidesAFileSecretAndTheEnvironmentCarriesOnlyAPath is the credential half of T4, and it
// is the assertion the task states as "the GitHub App key is a handle".
//
// What the control-plane receives is a PATH — the container's mount point, not the operator's host path,
// because a host path interpolated into a container names a file that is not there. The key's BYTES go to a
// 0600 file `docker inspect` cannot show, `ps` cannot show, and `compose config` cannot print.
func TestTheGitHubAppKeyRidesAFileSecretAndTheEnvironmentCarriesOnlyAPath(t *testing.T) {
	clearAppEnv(t)
	hostKey := filepath.Join(t.TempDir(), "github-app.pem")
	if err := os.WriteFile(hostKey, []byte(testAppKey), 0o600); err != nil {
		t.Fatal(err)
	}
	p := tempPaths(t)
	if warns := applyGitHubAppEnv(p, envGetter(appEnv(t, hostKey))); len(warns) != 0 {
		t.Fatalf("a fully configured GitHub App warned: %v", warns)
	}

	// 1. The environment the container will read.
	if got := os.Getenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE"); got != containerGitHubAppKeyPath {
		t.Fatalf("PALAI_GITHUB_APP_PRIVATE_KEY_FILE=%q, want the container mount %s — the .env.local value is a "+
			"HOST path and the control-plane would find nothing there", got, containerGitHubAppKeyPath)
	}
	if got := os.Getenv("PALAI_GITHUB_APP_ID"); got != "123456" {
		t.Fatalf("PALAI_GITHUB_APP_ID=%q, want the configured id — without it the publisher stays nil", got)
	}
	if got := os.Getenv("PALAI_GITHUB_REPO"); got != "acme/widgets" {
		t.Fatalf("PALAI_GITHUB_REPO=%q, want owner/repo — without it every approved pull request answers "+
			"'no pull-request client wired'", got)
	}
	// 2. No exported variable carries the key material. The sweep is over the WHOLE environment rather than
	// the four names above, because the failure this guards against is a fourth export nobody remembered.
	for _, kv := range os.Environ() {
		if strings.Contains(kv, "NOT-A-REAL-KEY-e22t4") || strings.Contains(kv, "BEGIN RSA PRIVATE KEY") {
			name, _, _ := strings.Cut(kv, "=")
			t.Fatalf("%s carries the App private key's bytes: an environment value is visible in `docker inspect` "+
				"and to every process in the container", name)
		}
	}
	// 3. The bytes are in the file secret, and only the owner can read it.
	staged, err := os.ReadFile(p.secretPath(gitHubAppKeySlot))
	if err != nil {
		t.Fatalf("the key was not staged into the file-secret slot: %v — compose mounts that path, so the "+
			"control-plane would read an empty key and publication would be disabled", err)
	}
	if string(staged) != testAppKey {
		t.Fatal("the staged key does not match the operator's file")
	}
	info, err := os.Stat(p.secretPath(gitHubAppKeySlot))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the staged App key is mode %o, want 0600", perm)
	}
}

// TestTheRepositorySlugFallsBackToTheCloneURL: §0.2 asks for PALAI_GIT_REPO, but an operator who set only
// the clone URL has already told us the answer. One derivation, shared with the repository binding's
// identity, so the App and the binding can never name two different repositories.
func TestTheRepositorySlugFallsBackToTheCloneURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"explicit", map[string]string{"PALAI_GIT_REPO": "acme/widgets"}, "acme/widgets"},
		{"from clone url", map[string]string{"PALAI_GIT_CLONE_URL": "https://github.com/acme/widgets.git"}, "acme/widgets"},
		{"neither", nil, ""},
		// A bare name is not owner/repo; guessing an owner would open pull requests somewhere else.
		{"bare name", map[string]string{"PALAI_GIT_REPO": "widgets"}, ""},
	} {
		if got := repositorySlug(envGetter(tc.env)); got != tc.want {
			t.Fatalf("%s: repositorySlug = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestComposeMountsTheGitHubAppKeyAsAFileSecret is the other side of the same wire, and it is read off the
// shipped compose file rather than trusted: `palai up` staging a key into a slot nothing mounts would be the
// silent skip again, one layer down.
func TestComposeMountsTheGitHubAppKeyAsAFileSecret(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	compose := string(raw)
	for _, want := range []string{
		"PALAI_GITHUB_APP_ID: ${PALAI_GITHUB_APP_ID:-}",
		"PALAI_GITHUB_APP_INSTALLATION_ID: ${PALAI_GITHUB_APP_INSTALLATION_ID:-}",
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE: " + containerGitHubAppKeyPath,
		"- github_app_key",
		`file: "${PALAI_HOME}/secrets/` + gitHubAppKeySlot + `"`,
	} {
		if !strings.Contains(compose, want) {
			t.Fatalf("compose.yaml does not carry %q, so the control-plane never receives the GitHub App and an "+
				"approved push waits forever", want)
		}
	}
	// The KEY PATH is written literally, exactly as the master key's is. Interpolating it would pass the
	// operator's HOST path into the container, where it names nothing.
	if strings.Contains(compose, "PALAI_GITHUB_APP_PRIVATE_KEY_FILE: ${") {
		t.Fatal("PALAI_GITHUB_APP_PRIVATE_KEY_FILE is interpolated from the invoking shell: that value is a HOST " +
			"path, and the container would look for the key at a name that means something else there")
	}
	// And the slot exists on every stack, or `compose up` fails outright on a missing mount source.
	p := tempPaths(t)
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("ensure the secret slots: %v", err)
	}
	if _, err := os.Stat(p.secretPath(gitHubAppKeySlot)); err != nil {
		t.Fatalf("no %s slot after a bring-up: compose names it unconditionally, so `docker compose up` would "+
			"fail on the missing mount source: %v", gitHubAppKeySlot, err)
	}
}
