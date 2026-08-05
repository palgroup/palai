// The install gate. It execs the real scripts/install/install.sh against a local file server, because
// the thing being protected is a script an autoscaler runs on a machine nobody is logged into — and a
// test that reimplemented its logic would prove the reimplementation.
//
// FOUR OF THESE SIX CASES ARE REFUSALS, and that ratio is the point. A happy-path installer test is
// nearly free to pass: the failure that matters is an installer that installs SOMETHING when it should
// have installed nothing, on a machine with no human to notice. So every refusal case asserts the
// binary is absent afterwards, not merely that the exit code was non-zero.
package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testVersion = "9.9.9"

// triple is the platform the script will detect on THIS machine. Hard-coding one would make the test
// pass on the author's laptop and serve a 404 in CI.
func triple() string { return runtime.GOOS + "-" + runtime.GOARCH }

// fakeArchive builds a .tar.gz holding a `palai` that answers --version, which is what the idempotence
// branch reads. It is a shell script rather than a compiled binary so the test costs milliseconds.
func fakeArchive(t *testing.T, version string) []byte {
	t.Helper()
	body := "#!/bin/sh\n[ \"$1\" = \"--version\" ] && echo " + version + "\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "palai", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// server serves the release layout the script expects. manifestFor lets a case serve a digest that does
// not match what it also serves as the archive — which is the whole point of fetching them separately.
func server(t *testing.T, archive []byte, manifest string) *httptest.Server {
	t.Helper()
	name := fmt.Sprintf("palai-%s-%s.tar.gz", testVersion, triple())
	mux := http.NewServeMux()
	mux.HandleFunc("/latest.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, testVersion)
	})
	mux.HandleFunc("/"+testVersion+"/"+name, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	mux.HandleFunc("/"+testVersion+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, manifest)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func honestManifest(archive []byte) string {
	return fmt.Sprintf("%s  palai-%s-%s.tar.gz\n", sha256hex(archive), testVersion, runtime.GOOS+"-"+runtime.GOARCH)
}

// run execs the installer. extraPath, when set, is prepended to PATH so a case can shadow `uname`.
func run(t *testing.T, script, baseURL, prefix, extraPath string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", script)
	env := []string{
		"PALAI_INSTALL_BASE_URL=" + baseURL,
		"PALAI_INSTALL_PREFIX=" + prefix,
		"HOME=" + t.TempDir(),
	}
	path := os.Getenv("PATH")
	if extraPath != "" {
		path = extraPath + string(os.PathListSeparator) + path
	}
	cmd.Env = append(env, "PATH="+path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func installerPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("resolve install.sh: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("install.sh is not where this test expects it: %v", err)
	}
	return p
}

func TestTheInstallerPlacesTheBinaryAndReportsWhatItInstalled(t *testing.T) {
	archive := fakeArchive(t, testVersion)
	srv := server(t, archive, honestManifest(archive))
	prefix := t.TempDir()

	out, err := run(t, installerPath(t), srv.URL, prefix, "")
	if err != nil {
		t.Fatalf("the installer failed on the happy path: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "palai")); err != nil {
		t.Fatalf("no palai binary at the install prefix: %v\n%s", err, out)
	}
	// A fleet is audited from this line: without the version and the digest, "what is this machine
	// running" has to be answered by logging into it.
	for _, want := range []string{testVersion, sha256hex(archive), filepath.Join(prefix, "palai"), triple()} {
		if !strings.Contains(out, want) {
			t.Fatalf("the installer did not report %q, so an operator cannot tell what landed:\n%s", want, out)
		}
	}
}

func TestAnArchiveThatDoesNotMatchTheManifestInstallsNOTHING(t *testing.T) {
	archive := fakeArchive(t, testVersion)
	tampered := append(append([]byte{}, archive...), 'x') // same name, different bytes
	srv := server(t, tampered, honestManifest(archive))   // manifest still describes the ORIGINAL
	prefix := t.TempDir()

	out, err := run(t, installerPath(t), srv.URL, prefix, "")
	if err == nil {
		t.Fatalf("a tampered archive was INSTALLED — the separate manifest bought nothing:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(prefix, "palai")); statErr == nil {
		t.Fatalf("the installer refused but left a binary behind, which is the worse half of the failure:\n%s", out)
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("the refusal does not say the digest disagreed, so the operator will look at the network:\n%s", out)
	}
}

func TestAManifestThatDoesNotNameThisArtifactIsRefused(t *testing.T) {
	archive := fakeArchive(t, testVersion)
	// A manifest for a DIFFERENT platform. The selection must be by exact filename: a substring match
	// would hand this digest to a machine whose name happens to share a prefix.
	srv := server(t, archive, fmt.Sprintf("%s  palai-%s-plan9-mips.tar.gz\n", sha256hex(archive), testVersion))
	prefix := t.TempDir()

	out, err := run(t, installerPath(t), srv.URL, prefix, "")
	if err == nil {
		t.Fatalf("an artifact absent from the manifest was installed:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(prefix, "palai")); statErr == nil {
		t.Fatalf("refused, and yet a binary is present:\n%s", out)
	}
}

// TestATruncatedDownloadInstallsNOTHING is the reason the script is one function invoked on its last
// line. It is not a hypothetical: `curl | sh` over a flaky link delivers a prefix, and a script written
// as top-level statements executes that prefix.
func TestATruncatedDownloadInstallsNOTHING(t *testing.T) {
	full, err := os.ReadFile(installerPath(t))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	cut := full[:len(full)*3/5]
	if bytes.Contains(cut, []byte("\nmain \"$@\"")) {
		t.Fatal("the truncation point still contains the invocation, so this case proves nothing — cut earlier")
	}
	partial := filepath.Join(t.TempDir(), "install-partial.sh")
	if err := os.WriteFile(partial, cut, 0o755); err != nil {
		t.Fatalf("write partial: %v", err)
	}

	archive := fakeArchive(t, testVersion)
	srv := server(t, archive, honestManifest(archive))
	prefix := t.TempDir()

	out, _ := run(t, partial, srv.URL, prefix, "")
	if _, statErr := os.Stat(filepath.Join(prefix, "palai")); statErr == nil {
		t.Fatalf("a HALF-DOWNLOADED installer installed a binary:\n%s", out)
	}
}

func TestAnUnsupportedArchitectureIsRefusedByName(t *testing.T) {
	shadow := t.TempDir()
	fake := filepath.Join(shadow, "uname")
	// Shadow `uname` rather than trusting that CI has an exotic machine. -s must still answer honestly,
	// so only the architecture is unsupported and the message has to name that one.
	body := "#!/bin/sh\ncase \"$1\" in\n  -m) echo mips64 ;;\n  *) exec /usr/bin/uname \"$@\" ;;\nesac\n"
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake uname: %v", err)
	}

	archive := fakeArchive(t, testVersion)
	srv := server(t, archive, honestManifest(archive))
	prefix := t.TempDir()

	out, err := run(t, installerPath(t), srv.URL, prefix, shadow)
	if err == nil {
		t.Fatalf("an unsupported architecture was accepted:\n%s", out)
	}
	if !strings.Contains(out, "mips64") {
		t.Fatalf("the refusal does not name the architecture it saw, so the operator cannot tell what the machine reported:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(prefix, "palai")); statErr == nil {
		t.Fatalf("refused, and yet a binary is present:\n%s", out)
	}
}

// TestRunningTheInstallerTwiceChangesNothingTheSecondTime matters because a provisioner may run this on
// every boot. Rewriting the file a running service holds open is how an idempotent-looking installer
// causes a restart nobody asked for.
func TestRunningTheInstallerTwiceChangesNothingTheSecondTime(t *testing.T) {
	archive := fakeArchive(t, testVersion)
	srv := server(t, archive, honestManifest(archive))
	prefix := t.TempDir()
	dest := filepath.Join(prefix, "palai")

	if out, err := run(t, installerPath(t), srv.URL, prefix, ""); err != nil {
		t.Fatalf("first run failed: %v\n%s", err, out)
	}
	first, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat after first run: %v", err)
	}

	out, err := run(t, installerPath(t), srv.URL, prefix, "")
	if err != nil {
		t.Fatalf("second run failed: %v\n%s", err, out)
	}
	second, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat after second run: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Fatalf("the second run REPLACED an identical binary (mtime %s -> %s); a service holding it open would be disturbed for nothing:\n%s",
			first.ModTime(), second.ModTime(), out)
	}
	if !strings.Contains(out, "already installed") {
		t.Fatalf("the second run did not say it was a no-op:\n%s", out)
	}
}
