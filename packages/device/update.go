package device

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// SelfUpdate moves this machine to a named version.
//
// IT IS THE ACTION HALF OF A FACT THE PLANE ALREADY HAD. `runners.agent_version` has always reported
// what a machine runs, so an operator could SEE drift and could do nothing about it without logging in —
// which is the same shape as every other gap this fleet had: a reading with no writing.
//
// ‼️ THE BINARY IS PROVEN BEFORE IT IS INSTALLED, and that check is the whole reason this is safe to run
// unattended. A machine that swaps in a broken binary is a machine that stops coming back, and nobody is
// watching it. So the downloaded archive must match its published digest AND the extracted binary must
// answer `--version` with the version that was asked for. Either failing leaves the running binary
// exactly where it was.
type SelfUpdate struct {
	// TargetVersion is what the deployment asked this machine to run.
	TargetVersion string
	// BaseURL is where releases live. It is a SETTING rather than a constant because an air-gapped fleet
	// mirrors them, and because a machine that cannot reach the internet must still be updatable.
	BaseURL string
	// InstallDir holds the binaries this machine runs. Both are replaced together: an agent at N+1 beside
	// a daemon at N is a combination nobody tested.
	InstallDir string
	// Privileged is the step that puts the staged binaries where the SERVICE reads them and restarts the
	// job — `palai agentd install` in production, nil on a machine that has no privileged half.
	//
	// ‼️ IT IS A SEAM AND NOT A CALL INTO macagent, because this package must not depend on the installer:
	// packages/device is what a Linux box runs too, and there is no account daemon there. A nil hook is a
	// machine with nothing privileged to move, not a machine that skipped it.
	Privileged func(ctx context.Context) error
	// Now, Client and Runner are seams. A test drives every path of this without a network, a release, or
	// the right to write into /usr/local.
	Client *http.Client
	Runner func(ctx context.Context, name string, args ...string) (string, error)
}

// Triple is the platform pair a release archive is named for, spelled the way scripts/install/install.sh
// spells it — one convention, so a machine downloads the file that installer would have.
func Triple() string { return runtime.GOOS + "-" + runtime.GOARCH }

// Apply performs the update and reports whether anything changed.
func (u SelfUpdate) Apply(ctx context.Context, currentVersion string) (bool, error) {
	target := strings.TrimSpace(u.TargetVersion)
	switch {
	case target == "":
		return false, nil
	case target == strings.TrimSpace(currentVersion):
		// Already there. Reported as "nothing changed" rather than as an error, because a fleet where
		// every machine is on the target is the state this feature exists to reach.
		return false, nil
	case strings.TrimSpace(u.BaseURL) == "":
		return false, fmt.Errorf("update to %s: no release base URL is configured on this machine", target)
	case u.InstallDir == "":
		return false, fmt.Errorf("update to %s: this machine does not know where its binaries live", target)
	}

	client := u.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	work, err := os.MkdirTemp("", "palai-update-*")
	if err != nil {
		return false, fmt.Errorf("update to %s: %w", target, err)
	}
	defer os.RemoveAll(work)

	name := fmt.Sprintf("palai-%s-%s.tar.gz", target, Triple())
	base := strings.TrimRight(u.BaseURL, "/") + "/" + target
	archive := filepath.Join(work, name)
	if err := download(ctx, client, base+"/"+name, archive); err != nil {
		return false, fmt.Errorf("update to %s: %w", target, err)
	}
	// THE DIGEST COMES IN ITS OWN REQUEST, exactly as the installer's does: a checksum served beside the
	// file it protects, by whoever served the file, protects nothing.
	sums := filepath.Join(work, "checksums.txt")
	if err := download(ctx, client, base+"/checksums.txt", sums); err != nil {
		return false, fmt.Errorf("update to %s: %w", target, err)
	}
	if err := verifyDigest(archive, name, sums); err != nil {
		return false, fmt.Errorf("update to %s: %w", target, err)
	}

	run := u.Runner
	if run == nil {
		run = execRun
	}
	if _, err := run(ctx, "tar", "-xzf", archive, "-C", work); err != nil {
		return false, fmt.Errorf("update to %s: extract: %w", target, err)
	}
	staged := filepath.Join(work, "palai")
	if _, err := os.Stat(staged); err != nil {
		return false, fmt.Errorf("update to %s: the archive carries no palai binary", target)
	}
	// ‼️ THE PROOF. A binary that cannot say its own version cannot be trusted to run this machine, and
	// this is the one moment the old one is still in place to fall back on.
	if err := os.Chmod(staged, 0o755); err != nil {
		return false, fmt.Errorf("update to %s: %w", target, err)
	}
	answered, err := run(ctx, staged, "--version")
	if err != nil {
		return false, fmt.Errorf("update to %s: the downloaded binary does not run: %w", target, err)
	}
	if got := strings.TrimSpace(answered); got != target {
		return false, fmt.Errorf("update to %s: the downloaded binary reports %q — the release is not the "+
			"one that was asked for, and the running binary is left in place", target, got)
	}

	// ‼️ EVERY BINARY THE ARCHIVE CARRIES, AND THE LIST USED TO BE ONE SHORT. palai-session-worker was
	// added to the device archive after this function was written and never added here, so an updated
	// Mac would have run its sessions' commands through a worker from the previous release — or, on a
	// machine updating past the release that introduced it, through no worker at all.
	for _, name := range []string{"palai-agentd", "palai-session-worker"} {
		if payload := filepath.Join(work, name); fileExists(payload) {
			if err := replace(payload, filepath.Join(u.InstallDir, name)); err != nil {
				return false, fmt.Errorf("update to %s: %w", target, err)
			}
		}
	}

	// ‼️ AND THE PRIVILEGED HALF, WITHOUT WHICH THIS FUNCTION MOVED NOTHING THAT MATTERS. The lines above
	// replace the PAYLOAD COPIES that sit beside the agent; the daemon launchd actually runs lives at
	// macagent.InstalledBinaryPath and the job holds its image until it is restarted. So an operator who
	// moved a device to a new version through the admin plane got a NEW agent and the OLD daemon still
	// serving — and the daemon is the process that mints session accounts and starts their workers.
	//
	// It runs BEFORE the agent is replaced, because replacing the agent is what ends this process: a
	// privileged step queued after it would never run. And a failure here FAILS THE UPDATE rather than
	// being logged past, because a machine whose daemon did not move is not at the target version, and
	// the admin plane must not be told it is.
	if u.Privileged != nil {
		if err := u.Privileged(ctx); err != nil {
			return false, fmt.Errorf("update to %s: the binaries were staged but the privileged install did not "+
				"run, so the daemon launchd is serving is still the previous one: %w", target, err)
		}
	}

	// The agent LAST, and its own replacement is what ends the process.
	if err := replace(staged, filepath.Join(u.InstallDir, "palai")); err != nil {
		return false, fmt.Errorf("update to %s: %w", target, err)
	}
	return true, nil
}

func download(ctx context.Context, client *http.Client, url, dest string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: %s", url, response.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, response.Body)
	return err
}

// verifyDigest selects the line by EXACT filename: a substring match would accept a darwin-arm64 digest
// for a linux-amd64 archive, which is the installer's own recorded reasoning.
func verifyDigest(archive, name, sums string) error {
	raw, err := os.ReadFile(sums)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no digest for %s in the manifest", name)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("digest mismatch for %s: got %s, want %s", name, got, want)
	}
	return nil
}

// replace writes beside the target and renames, so an interrupted update cannot leave a partial binary
// where a whole one was — the same discipline install.sh follows.
func replace(staged, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".incoming"
	in, err := os.Open(staged)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func execRun(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}
