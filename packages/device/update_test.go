package device

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// channel serves one release the way scripts/install/install.sh expects to find it: the archive under
// its version, and the digests as their own document.
func channel(t *testing.T, version string, archive []byte, digest string) *httptest.Server {
	t.Helper()
	name := fmt.Sprintf("palai-%s-%s.tar.gz", version, Triple())
	mux := http.NewServeMux()
	mux.HandleFunc("/"+version+"/"+name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/"+version+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", digest, name)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sha(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// installed lays a directory holding the binary this machine is running, so a refusal can be shown to
// have left it exactly as it was.
func installed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "palai"), []byte("the running binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func running(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "palai"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestAnUpdateThatDOESNOTMatchItsDigestLeavesTheMachineAlone — A MACHINE NOBODY IS WATCHING MUST COME
// BACK.
//
// The archive is served under the right name at the right version and its bytes do not match the
// published digest. Nothing about the running binary may change: the whole reason this can run
// unattended on a hundred machines is that a bad download is a no-op rather than a brick.
func TestAnUpdateThatDOESNOTMatchItsDigestLeavesTheMachineAlone(t *testing.T) {
	dir := installed(t)
	srv := channel(t, "v2", []byte("tampered"), sha([]byte("the real archive")))

	changed, err := SelfUpdate{TargetVersion: "v2", BaseURL: srv.URL, InstallDir: dir}.Apply(context.Background(), "v1")
	if err == nil {
		t.Fatal("a digest mismatch must refuse")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("the refusal does not say the digest was wrong: %v", err)
	}
	if changed {
		t.Fatal("Apply reported a change after refusing")
	}
	if got := running(t, dir); got != "the running binary" {
		t.Fatalf("the running binary was replaced by a refused download: %q", got)
	}
}

// TestAnUpdateWhoseBinaryCannotSayItsVersionIsRefused — THE PROOF THAT MAKES THIS SAFE.
//
// A digest only says the bytes are the ones that were published. It cannot say they RUN. So the
// extracted binary is asked its version and must answer the version that was requested; anything else —
// a binary that crashes, one that hangs, one from a different release — leaves the machine on what it
// has. This is the one moment the old binary is still there to fall back on.
func TestAnUpdateWhoseBinaryCannotSayItsVersionIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, answers, wants string }{
		{"a binary that reports another release", "v3", "reports"},
		{"a binary that does not run at all", "", "does not run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := installed(t)
			archive := []byte("archive bytes")
			srv := channel(t, "v2", archive, sha(archive))

			u := SelfUpdate{
				TargetVersion: "v2", BaseURL: srv.URL, InstallDir: dir,
				Runner: func(_ context.Context, name string, args ...string) (string, error) {
					if filepath.Base(name) == "tar" {
						// The extraction "succeeds" and lays down a binary, so the probe below is what
						// decides — which is the property under test.
						return "", os.WriteFile(filepath.Join(args[len(args)-1], "palai"), []byte("new"), 0o755)
					}
					if tc.answers == "" {
						return "", fmt.Errorf("exec format error")
					}
					return tc.answers, nil
				},
			}
			changed, err := u.Apply(context.Background(), "v1")
			if err == nil {
				t.Fatal("an unprovable binary must be refused")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("refusal %q does not name why", err)
			}
			if changed {
				t.Fatal("Apply reported a change after refusing")
			}
			if got := running(t, dir); got != "the running binary" {
				t.Fatalf("the running binary was replaced by an unprovable download: %q", got)
			}
		})
	}
}

// TestAMachineAlreadyOnTheTargetDoesNothing — a fleet where every machine is on the target is the state
// this feature exists to reach, so it must not be an error and must not re-download on every poll.
func TestAMachineAlreadyOnTheTargetDoesNothing(t *testing.T) {
	dir := installed(t)
	changed, err := SelfUpdate{TargetVersion: "v1", BaseURL: "http://127.0.0.1:1", InstallDir: dir}.
		Apply(context.Background(), "v1")
	if err != nil {
		t.Fatalf("a machine already on the target must not error: %v", err)
	}
	if changed {
		t.Fatal("Apply reported a change for a machine that was already there")
	}
}

// TestNoReleaseBaseIsARefusalAndNotAGuess — an air-gapped machine that reached out to the internet
// because nobody told it not to is the opposite of what an air gap is for.
func TestNoReleaseBaseIsARefusalAndNotAGuess(t *testing.T) {
	_, err := SelfUpdate{TargetVersion: "v2", InstallDir: installed(t)}.Apply(context.Background(), "v1")
	if err == nil || !strings.Contains(err.Error(), "no release base URL") {
		t.Fatalf("a machine with no base must refuse by name, got %v", err)
	}
}
