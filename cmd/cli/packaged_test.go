// packaged_test.go is the OUTSIDE-THE-TREE contract: a `palai` binary handed to an operator has no
// repo behind it, and until this test existed every compose-driving command resolved
// deploy/compose/compose.yaml RELATIVE TO CWD — so the only way to run Palai was to clone the repo
// and stay inside it (docs/research/deployment-and-product-shape.md).
//
// Every case here runs the BUILT BINARY with its working directory in a temp dir that has no
// `deploy/` at any level above it, and with every PALAI_* variable stripped from the environment.
// A case that ran from the repo root would prove nothing — that is exactly the bug.
//
// WHAT IS PROVEN HERE: compose-file resolution, the materialisation of the embedded deploy files,
// `config validate` against them, and the image-reference contract (an honest refusal with no
// published default; the operator's PALAI_*_IMAGE overrides getting through).
//
// WHAT IS NOT PROVEN HERE: that the stack actually COMES UP from outside the tree. That needs a
// Docker daemon and three real images, and no image has ever been published (release.yml's publish
// leg is a credential-less dry run), so the packaged bring-up can only be driven with locally-built
// images — see the report accompanying this change. This file stops at the last assertion that can
// be made without a daemon, and says so rather than pretending.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildCLI builds the CLI under test once per `go test` process and returns its path — linking it
// per test case cost more than every assertion here put together.
var buildCLI = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "palai-packaged")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "palai")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("build palai: %w\n%s", err, out)
	}
	return bin, nil
})

func cli(t *testing.T) string {
	t.Helper()
	bin, err := buildCLI()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

// outsideTheTree returns a working directory with no `deploy/` at ANY level above it, and fails the
// test if that is not true — a temp dir that happened to sit under a checkout would make every
// assertion below vacuous. Symlinks are resolved first (/var -> /private/var on macOS) so the walk
// is over real directories.
func outsideTheTree(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "deploy", "compose")); err == nil {
			t.Fatalf("temp dir %s has a deploy/compose above it at %s — the outside-the-tree case would be vacuous", dir, d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// palai runs the built CLI with cwd=dir and an environment with EVERY PALAI_* variable stripped
// (the outer `go test` may carry PALAI_HOME or PALAI_COMPOSE_FILE) and every key extra defines
// removed, so extra always wins — a duplicate PATH would otherwise be resolved by whichever copy
// the runtime happened to keep.
func palai(t *testing.T, bin, dir string, extra []string, args ...string) (string, error) {
	t.Helper()
	overridden := map[string]bool{}
	for _, kv := range extra {
		if k, _, ok := strings.Cut(kv, "="); ok {
			overridden[k] = true
		}
	}
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "PALAI_") || overridden[k] {
			continue
		}
		env = append(env, kv)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// fakeDocker puts a recording `docker` on PATH and returns the PATH entry plus a reader for what the
// CLI invoked. It exists for two reasons. The obvious one is that these cases assert on the compose
// COMMAND LINE, which is the only place the --build contract is observable. The other is that the
// real `docker` CLI on macOS STARTS DOCKER DESKTOP when the daemon is down — a test tier that boots
// a VM on a developer's machine (and takes 40s per case doing it) is not one that belongs in
// `make verify`.
//
// It fails every compose call, which is where the CLI stops: everything under test — resolution,
// materialisation, image references, --build — has already happened by the time compose is reached.
func fakeDocker(t *testing.T) (pathEntry, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "docker.log")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf 'arg\\t%s\\n' \"$a\"; done >> \"" + log + "\"\n" +
		"env | sed -n 's/^\\(PALAI_[A-Z_]*IMAGE=.*\\)$/env\\t\\1/p' >> \"" + log + "\"\n" +
		"case \"$1\" in\n" +
		"  build) exit 0 ;;\n" +
		// `docker image inspect --format {{.Id}}` — the CLI requires a bare sha256 config digest.
		"  image) echo sha256:" + strings.Repeat("a", 64) + " ; exit 0 ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return dir, log
}

// dockerSaw reports whether the recorded invocations contain a line exactly equal to kind+"\t"+value
// — whole-token equality, never a substring: "--build" is a substring of "--no-build", and a
// substring assertion here would pass on the exact confusion these cases exist to catch.
func dockerSaw(t *testing.T, log, kind, value string) bool {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read the recorded docker invocations: %v", err)
	}
	want := kind + "\t" + value
	for _, line := range strings.Split(string(raw), "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// TestPackagedCLIRunsWithNoSourceTree is the whole point: init, materialise, validate and reach the
// image contract, all from a directory with no deploy/ above it.
func TestPackagedCLIRunsWithNoSourceTree(t *testing.T) {
	bin := cli(t)
	work := outsideTheTree(t)
	home := filepath.Join(work, ".palai")
	env := []string{"PALAI_HOME=" + home}

	if out, err := palai(t, bin, work, env, "init"); err != nil {
		t.Fatalf("palai init outside the tree: %v\n%s", err, out)
	}

	// `local up` must get as far as the IMAGE contract. It must not die on a missing compose file,
	// and it must not silently fall through to compose's `:local` build tag — outside a checkout
	// that tag can only mean a pull of something that is not this release.
	out, err := palai(t, bin, work, env, "local", "up")
	if err == nil {
		t.Fatalf("packaged `local up` succeeded with no published images and no override:\n%s", out)
	}
	if strings.Contains(out, "deploy/compose/compose.yaml") {
		t.Fatalf("packaged `local up` still resolves the compose file relative to cwd:\n%s", out)
	}
	if !strings.Contains(out, "no published image default") {
		t.Fatalf("packaged `local up` did not refuse on the image default; got:\n%s", out)
	}

	// The embedded deploy files must be real paths on disk under PALAI_HOME (`docker compose -f -`
	// reads stdin once and a production bring-up passes two files), and byte-identical to the
	// committed originals — a materialised copy that drifted would be a second source of truth.
	for _, name := range []string{"compose.yaml", "production.yml", "production-entrypoint.sh"} {
		got, err := os.ReadFile(filepath.Join(home, "compose", name))
		if err != nil {
			t.Fatalf("materialised %s: %v", name, err)
		}
		want, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "compose", name))
		if err != nil {
			t.Fatalf("committed %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("materialised %s differs from the committed deploy/compose/%s", name, name)
		}
	}
}

// TestPackagedBringUpNeverBuilds is requirement 4: a binary with no Dockerfiles cannot build, so the
// packaged bring-up must drive the MATERIALISED compose file with --no-build and with all three image
// references supplied. It also proves the published-default refusal is a DEFAULT and not a wall —
// the operator's pre-existing PALAI_*_IMAGE variables get through it.
func TestPackagedBringUpNeverBuilds(t *testing.T) {
	bin := cli(t)
	work := outsideTheTree(t)
	home := filepath.Join(work, ".palai")
	dockerPath, log := fakeDocker(t)
	env := []string{"PALAI_HOME=" + home, "PATH=" + dockerPath + ":" + os.Getenv("PATH")}

	if out, err := palai(t, bin, work, env, "init"); err != nil {
		t.Fatalf("palai init outside the tree: %v\n%s", err, out)
	}
	engineDigest := "sha256:" + strings.Repeat("b", 64)
	out, _ := palai(t, bin, work, append(env,
		"PALAI_CONTROL_PLANE_IMAGE=palai/control-plane:local",
		"PALAI_RUNNER_IMAGE=palai/runner:local",
		"PALAI_ENGINE_IMAGE="+engineDigest,
	), "local", "up")
	if strings.Contains(out, "no published image default") {
		t.Fatalf("PALAI_*_IMAGE overrides did not get past the published-default refusal:\n%s", out)
	}
	if !dockerSaw(t, log, "arg", "--no-build") {
		t.Fatalf("packaged bring-up did not pass --no-build; it has no build context. invoked:\n%s", read(t, log))
	}
	if dockerSaw(t, log, "arg", "--build") {
		t.Fatalf("packaged bring-up passed --build with no Dockerfiles on disk. invoked:\n%s", read(t, log))
	}
	if !dockerSaw(t, log, "arg", filepath.Join(home, "compose", "compose.yaml")) {
		t.Fatalf("packaged bring-up did not drive the materialised compose file. invoked:\n%s", read(t, log))
	}
	// The runner's lease rejects anything but a bare sha256 config digest, so the engine reference
	// compose is handed has to be the digest, not a repository tag.
	if !dockerSaw(t, log, "env", "PALAI_ENGINE_IMAGE="+engineDigest) {
		t.Fatalf("compose was not handed the engine digest. invoked:\n%s", read(t, log))
	}
	for _, want := range []string{"PALAI_CONTROL_PLANE_IMAGE=palai/control-plane:local", "PALAI_RUNNER_IMAGE=palai/runner:local"} {
		if !dockerSaw(t, log, "env", want) {
			t.Fatalf("compose was not handed %s — it would fall back to its :local build tag. invoked:\n%s", want, read(t, log))
		}
	}
}

// TestCheckoutStillBuildsAndMaterialisesNothing is the other half of the contract: inside a checkout
// NOTHING may change. The committed deploy/compose/compose.yaml is still what runs, --build is still
// passed, the reference engine is still built from source, and nothing is written under PALAI_HOME —
// a packaged fallback that also fired in a checkout would fork the compose file an operator debugs
// from the one they edit.
func TestCheckoutStillBuildsAndMaterialisesNothing(t *testing.T) {
	bin := cli(t)
	home := filepath.Join(t.TempDir(), ".palai")
	dockerPath, log := fakeDocker(t)
	env := []string{"PALAI_HOME=" + home, "PATH=" + dockerPath + ":" + os.Getenv("PATH")}
	root := repoRoot(t)

	if out, err := palai(t, bin, root, env, "init"); err != nil {
		t.Fatalf("palai init in the checkout: %v\n%s", err, out)
	}
	_, _ = palai(t, bin, root, env, "local", "up")

	if !dockerSaw(t, log, "arg", "--build") {
		t.Fatalf("a checkout stopped passing --build. invoked:\n%s", read(t, log))
	}
	if !dockerSaw(t, log, "arg", filepath.Join(root, "engines", "reference")) {
		t.Fatalf("a checkout stopped building the reference engine from source. invoked:\n%s", read(t, log))
	}
	if !dockerSaw(t, log, "arg", committedComposeFile) {
		t.Fatalf("a checkout stopped driving %s. invoked:\n%s", committedComposeFile, read(t, log))
	}
	if _, err := os.Stat(filepath.Join(home, "compose")); err == nil {
		t.Fatalf("a checkout materialised the embedded compose files into %s — the committed tree must win", home)
	}
}

// TestComposeFileOverrideStillWins pins the precedence the e2e harness, the UAT and every
// development checkout depend on: PALAI_COMPOSE_FILE is resolved FIRST and verbatim. Embedding is a
// fallback, so a set override must both be driven and suppress materialisation entirely — an
// embedded copy that quietly won would point an operator's compose at bytes they never edited.
func TestComposeFileOverrideStillWins(t *testing.T) {
	bin := cli(t)
	work := outsideTheTree(t)
	home := filepath.Join(work, ".palai")
	dockerPath, log := fakeDocker(t)

	// A compose file that is neither the checkout's nor the embedded copy's destination.
	chosen := filepath.Join(work, "elsewhere", "my-compose.yaml")
	if err := os.MkdirAll(filepath.Dir(chosen), 0o700); err != nil {
		t.Fatalf("make override dir: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatalf("read the committed compose file: %v", err)
	}
	if err := os.WriteFile(chosen, src, 0o600); err != nil {
		t.Fatalf("write the override compose file: %v", err)
	}

	env := []string{
		"PALAI_HOME=" + home,
		"PATH=" + dockerPath + ":" + os.Getenv("PATH"),
		"PALAI_COMPOSE_FILE=" + chosen,
	}
	if out, err := palai(t, bin, work, env, "init"); err != nil {
		t.Fatalf("palai init with an override: %v\n%s", err, out)
	}
	_, _ = palai(t, bin, work, append(env,
		"PALAI_CONTROL_PLANE_IMAGE=palai/control-plane:local",
		"PALAI_RUNNER_IMAGE=palai/runner:local",
		"PALAI_ENGINE_IMAGE=sha256:"+strings.Repeat("c", 64),
	), "local", "up")

	if !dockerSaw(t, log, "arg", chosen) {
		t.Fatalf("PALAI_COMPOSE_FILE was not the file driven. invoked:\n%s", read(t, log))
	}
	if _, err := os.Stat(filepath.Join(home, "compose")); err == nil {
		t.Fatalf("PALAI_COMPOSE_FILE was set and the embedded copies were materialised anyway into %s", home)
	}
}

// committedComposeFile is the path a checkout drives, spelled here the way the CLI passes it to
// compose (relative to cwd) rather than imported, so this file asserts against the contract and not
// against the implementation's own constant.
const committedComposeFile = "deploy/compose/compose.yaml"

func read(t *testing.T, path string) string {
	t.Helper()
	raw, _ := os.ReadFile(path)
	return string(raw)
}
