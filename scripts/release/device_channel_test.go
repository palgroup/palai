package release

// WHERE THE TWO HALVES OF THE DEVICE CONTRACT MEET: scripts/release/build.sh produces the archives and
// scripts/install/install.sh consumes them. Both have had their own gate for a while, and both were
// green while they disagreed — the builder emitted linux only, so a Mac fleet, which is most of the
// fleet this agent exists for, could not be served by a release at all.
//
// ‼️ NEITHER SIDE'S LIST IS WRITTEN DOWN HERE. The devices the fleet supports are the ones install.sh's
// detect_triple resolves, so this drives the REAL installer once per uname pair it accepts, against a
// REAL release directory served over HTTP. A target dropped from the builder's default fails as a 404;
// a triple the installer stops resolving fails at its own refusal. A hardcoded expectation in this file
// would have kept passing through exactly the divergence that shipped.

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// unameToTriple is the machine axis, in the raw values `uname -s` / `uname -m` report — the aliases
// included (aarch64, x86_64), because those are what real hosts say. The mapping to a release triple is
// install.sh's to make, not this test's: what is asserted below is that the archive the installer chose
// is the one whose binary lands on disk.
var unameToTriple = []struct{ os, machine, triple string }{
	{"Darwin", "arm64", "darwin-arm64"},
	{"Darwin", "x86_64", "darwin-amd64"},
	{"Linux", "aarch64", "linux-arm64"},
	{"Linux", "x86_64", "linux-amd64"},
}

// fakeUname puts a `uname` earlier on PATH that answers -s and -m with the given values, so one host can
// stand in for every device in the fleet. It answers nothing else: an installer that grew a third uname
// question would fail here rather than silently read this host's answer.
func fakeUname(t *testing.T, osName, machine string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n-s) echo " + osName + " ;;\n-m) echo " + machine + " ;;\n" +
		"*) echo \"fake uname: unexpected argument $1\" >&2; exit 64 ;;\nesac\n"
	path := filepath.Join(dir, "uname")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// serveVersion serves one release's device directory at /<version>/, which is the layout install.sh
// resolves: the archive and the checksum manifest are two separate requests under one immutable prefix.
func serveVersion(t *testing.T, dir, version string) string {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/"+version+"/", http.FileServer(http.Dir(dir))))
	t.Cleanup(srv.Close)
	return srv.URL
}

// binaryInArchive returns the sha256 of the `palai` member — the bytes an install is supposed to land.
func binaryInArchive(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip open %s: %v", path, err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read %s: %v", path, err)
		}
		if hdr.Name == "palai" {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return sha256Bytes(b)
		}
	}
	t.Fatalf("%s carries no `palai` member — the installer would install nothing", path)
	return ""
}

// TestAReleaseServesEveryDeviceTheInstallerCanResolve is T7's first claim, measured rather than
// declared. The release is built with the DEFAULT agent targets — the flag is deliberately absent,
// because the default is what a release cut by .github/workflows/release.yml uses and therefore the
// thing that can silently lose a platform.
func TestAReleaseServesEveryDeviceTheInstallerCanResolve(t *testing.T) {
	root := repoRoot(t)
	const version = "18.0.0"

	// Host-only CLI keeps this to the agent matrix; --agent-targets is omitted ON PURPOSE.
	out, _ := buildRelease(t, "--cli-targets", hostTarget(t))
	device := filepath.Join(out, "device")
	base := serveVersion(t, device, version)

	for _, d := range unameToTriple {
		t.Run(d.triple, func(t *testing.T) {
			prefix := t.TempDir()
			cmd := exec.Command("/bin/sh", filepath.Join(root, "scripts/install/install.sh"))
			cmd.Env = append(os.Environ(),
				"PATH="+fakeUname(t, d.os, d.machine)+string(os.PathListSeparator)+os.Getenv("PATH"),
				"PALAI_INSTALL_BASE_URL="+base,
				"PALAI_VERSION="+version,
				"PALAI_INSTALL_PREFIX="+prefix,
			)
			combined, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("install.sh could not install a %s device from a release built with the DEFAULT "+
					"agent targets — this platform is unreachable for the whole fleet:\n%s", d.triple, combined)
			}

			installed, err := os.ReadFile(filepath.Join(prefix, "palai"))
			if err != nil {
				t.Fatalf("install.sh reported success but installed nothing: %v\n%s", err, combined)
			}
			// The RIGHT archive, not merely AN archive: four archives share one manifest, and a selection
			// that matched by prefix rather than by exact name would install a binary that cannot exec.
			want := binaryInArchive(t, filepath.Join(device, "palai-"+version+"-"+d.triple+".tar.gz"))
			if got := sha256Bytes(installed); got != want {
				t.Fatalf("a %s install landed the binary from a DIFFERENT archive (%s, want %s)", d.triple, got, want)
			}
			if !strings.Contains(string(combined), d.triple) {
				t.Fatalf("the installer did not report which device it installed for:\n%s", combined)
			}
		})
	}
}

// hostTarget is this machine's <os>/<arch>, so the CLI leg of the build stays a single compile. It is
// not the subject of this test — the agent matrix is.
func hostTarget(t *testing.T) string {
	t.Helper()
	goos, err := exec.Command("go", "env", "GOOS").Output()
	if err != nil {
		t.Fatal(err)
	}
	goarch, err := exec.Command("go", "env", "GOARCH").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(goos)) + "/" + strings.TrimSpace(string(goarch))
}
