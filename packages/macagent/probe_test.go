package macagent_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/macagent"
)

// TestAProbeThatLosesTheColdBootRaceRetriesRatherThanRefusing is THE test this task exists to write,
// because it is where the cgo decision's bill is paid.
//
// Socket ACTIVATION would have made this untestable and unnecessary: launchd would hold the listener,
// a connection would start the job, and a caller could never arrive early. It needs
// `launch_activate_socket`, a C function with no wrapper in x/sys, and this tree has zero cgo — every
// release build is CGO_ENABLED=0 and CI is ubuntu-24.04 — so a cgo daemon would be a daemon CI never
// compiles. RunAtLoad+KeepAlive keeps boot-start and crash-restart and loses exactly one thing: the
// socket does not exist until the daemon has bound it.
//
// AND THAT WINDOW OPENS ON A FRESHLY BOOTED CLOUD MAC, which is the zero-touch path itself. A
// single-shot probe there reports "no daemon" and keeps a sound machine out of the pool — fail-closed
// in the right direction for entirely the wrong cause.
//
// ‼️ IT DRIVES THE SHIPPED POLICY BY NAME. Every count below comes from macagent.ColdBootProbe, so a
// change that quietly reduced the window to one attempt fails here rather than passing against numbers
// a test author picked. Only the CLOCK is substituted; the socket, the errno and the round trip are real.
func TestAProbeThatLosesTheColdBootRaceRetriesRatherThanRefusing(t *testing.T) {
	if macagent.ColdBootProbe.Attempts < 2 {
		t.Fatalf("ColdBootProbe.Attempts is %d: a policy that dials once is the single-shot probe this test exists to forbid",
			macagent.ColdBootProbe.Attempts)
	}

	t.Run("the socket node does not exist yet (ENOENT) and appears on the last attempt", func(t *testing.T) {
		sock := socketPath(t)
		prober, slept := lateBinding(t, sock, macagent.ColdBootProbe.Attempts, func() {
			startFakeDaemon(t, sock, "1.2.3", []int{3, 7})
		})

		health, err := prober.Probe(context.Background())
		if err != nil {
			t.Fatalf("a machine whose daemon bound its socket inside the cold-boot window was refused: %v", err)
		}
		if health.Attempts != macagent.ColdBootProbe.Attempts {
			t.Errorf("probe took %d attempts, want the shipped %d", health.Attempts, macagent.ColdBootProbe.Attempts)
		}
		// The round trip ANSWERED, and both halves of the answer came off the wire. A probe that merely
		// connected would report the same success against a daemon that says nothing.
		if got := fmt.Sprint(health.Slots); got != "[3 7]" {
			t.Errorf("health.Slots = %s, want [3 7] — the probe did not complete a list round trip", got)
		}
		if health.Version != "1.2.3" {
			t.Errorf("health.Version = %q, want the stamp the RUNNING daemon reported", health.Version)
		}
		if *slept != macagent.ColdBootProbe.Window() {
			t.Errorf("the probe waited %s across its retries, and the shipped window is %s", *slept, macagent.ColdBootProbe.Window())
		}
	})

	t.Run("the node exists with nothing bound (ECONNREFUSED) and is retried", func(t *testing.T) {
		sock := socketPath(t)
		orphanSocketNode(t, sock)
		// The exact errno a cold boot produces once launchd has made the path but the daemon has not
		// bound: measured here rather than asserted, by connecting to it.
		if _, err := net.Dial("unix", sock); !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("the fixture did not reproduce ECONNREFUSED, so this test measures nothing: %v", err)
		}

		prober, _ := lateBinding(t, sock, macagent.ColdBootProbe.Attempts, func() {
			_ = os.Remove(sock)
			startFakeDaemon(t, sock, "1.2.3", nil)
		})
		if _, err := prober.Probe(context.Background()); err != nil {
			t.Fatalf("a refused connection inside the window is a daemon that has not bound YET, and it was treated as absent: %v", err)
		}
	})

	t.Run("a socket that never answers is refused, and the refusal names the window", func(t *testing.T) {
		prober, slept := lateBinding(t, socketPath(t), 0, func() {})

		health, err := prober.Probe(context.Background())
		if !errors.Is(err, macagent.ErrNotAnswering) {
			t.Fatalf("a machine with no daemon at all must be refused with ErrNotAnswering, got %v", err)
		}
		if health.Attempts != macagent.ColdBootProbe.Attempts {
			t.Errorf("gave up after %d attempts, want the shipped %d", health.Attempts, macagent.ColdBootProbe.Attempts)
		}
		if *slept != macagent.ColdBootProbe.Window() {
			t.Errorf("waited %s before refusing, want the shipped window %s", *slept, macagent.ColdBootProbe.Window())
		}
		// The operator reads this on a machine that will not join a pool. A window it does not name is a
		// window nobody can tell from a single-shot probe.
		if !strings.Contains(err.Error(), macagent.ColdBootProbe.Window().String()) {
			t.Errorf("the refusal does not name the window it waited (%s): %v", macagent.ColdBootProbe.Window(), err)
		}
	})

	t.Run("a failure a retry cannot fix is refused at once", func(t *testing.T) {
		// EACCES is this process not being in the `palai` group. Group membership is the ENTIRE
		// credential, so waiting the window out cannot change it — and a probe that spent forty seconds
		// on it would hide the one-line fix behind a delay that reads like a broken machine.
		prober := macagent.NewProber(socketPath(t))
		prober.Sleep = func(time.Duration) {
			t.Error("a permission failure was retried; no amount of waiting adds this process to a group")
		}
		prober.Dial = func(context.Context) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.EACCES}
		}

		health, err := prober.Probe(context.Background())
		if err == nil {
			t.Fatal("a socket this process cannot open was admitted")
		}
		if errors.Is(err, macagent.ErrNotAnswering) {
			t.Errorf("EACCES was reported as `not answering`, which sends an operator looking for a daemon that is right there: %v", err)
		}
		if health.Attempts != 1 {
			t.Errorf("dialled %d times for a failure no retry can fix, want 1", health.Attempts)
		}
	})

	t.Run("a daemon that answers a classed refusal ends the probe", func(t *testing.T) {
		sock := socketPath(t)
		startRefusingDaemon(t, sock, macagent.ClassUnsupported, "session accounts exist only on macOS")
		prober := macagent.NewProber(sock)
		prober.Sleep = func(time.Duration) { t.Error("a daemon that ANSWERED was waited on as though it were still booting") }

		_, err := prober.Probe(context.Background())
		if err == nil {
			t.Fatal("a refused list was read as a healthy daemon")
		}
		var classed *macagent.Error
		if !errors.As(err, &classed) || classed.Class != macagent.ClassUnsupported {
			t.Errorf("the daemon's class did not survive to the caller, who branches on it: %v", err)
		}
	})
}

// TestTheVersionAProbeReportsComesOffTheSocketNotTheDisk pins which of two divergent readings is
// authority, because the divergence is the whole reason an upgrade decision can be wrong: a copy that
// landed at InstalledBinaryPath and a launchd job that was never restarted leave a NEW FILE and an OLD
// DAEMON, and only one of them is deciding whether an account gets created.
func TestTheVersionAProbeReportsComesOffTheSocketNotTheDisk(t *testing.T) {
	sock := socketPath(t)
	startFakeDaemon(t, sock, "0.9.0-old", nil)
	// The file on disk says something else entirely, and nothing here reads it.
	onDisk := filepath.Join(filepath.Dir(sock), "palai-agentd")
	if err := os.WriteFile(onDisk, []byte("2.0.0-new"), 0o755); err != nil {
		t.Fatal(err)
	}

	health, err := macagent.NewProber(sock).Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if health.Version != "0.9.0-old" {
		t.Errorf("Health.Version = %q, want the RUNNING daemon's stamp %q — the binary on disk is not what serves a request",
			health.Version, "0.9.0-old")
	}
}

// TestADaemonTooOldToNameItselfIsStillAdmitted: the version verb arrived in Task 2, so a Task 1 daemon
// answers `err unknown_verb`. That is a reason to upgrade and not a reason to be unreachable — a machine
// that lists is a machine that isolates.
func TestADaemonTooOldToNameItselfIsStillAdmitted(t *testing.T) {
	sock := socketPath(t)
	startDaemon(t, sock, func(line string) string {
		if strings.HasPrefix(line, "version") {
			return "err unknown_verb \"version\" is not one of create, delete, list\n"
		}
		return "ok list\n"
	})

	health, err := macagent.NewProber(sock).Probe(context.Background())
	if err != nil {
		t.Fatalf("a daemon that cannot name itself was refused: %v", err)
	}
	if health.Version != "" {
		t.Errorf("Health.Version = %q, want empty — an unknown verb means older than the first build that could say", health.Version)
	}
}

// socketPath returns a short-enough path for a unix socket. Not t.TempDir(): it embeds the test's name,
// and these names are long enough to pass macOS's 104-byte sun_path limit, which surfaces as a bind
// error nobody would connect to the name of the test.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agentd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// lateBinding wraps the SHIPPED prober so the daemon appears on attempt `appearsOn` (0 = never), and
// returns a pointer to the total time it would have waited. Only the clock is replaced: the socket, the
// dial and the errno are real.
func lateBinding(t *testing.T, sock string, appearsOn int, start func()) (*macagent.Prober, *time.Duration) {
	t.Helper()
	prober := macagent.NewProber(sock)
	real := prober.Dial
	dials := 0
	var slept time.Duration
	prober.Sleep = func(d time.Duration) { slept += d }
	prober.Dial = func(ctx context.Context) (net.Conn, error) {
		dials++
		if appearsOn > 0 && dials == appearsOn {
			start()
		}
		return real(ctx)
	}
	return prober, &slept
}

// orphanSocketNode leaves a socket file with nothing listening on it — the state a machine is in
// between launchd creating the path and the daemon binding. SetUnlinkOnClose(false) is what keeps the
// node behind, since Go removes it on Close by default.
func orphanSocketNode(t *testing.T, sock string) {
	t.Helper()
	addr, err := net.ResolveUnixAddr("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(sock) })
}

// startFakeDaemon answers `list` and `version` the way palai-agentd does, in RAW WIRE BYTES rather than
// through the encoder, so the client's parser is measured against literal text rather than against its
// own round trip.
func startFakeDaemon(t *testing.T, sock, stamp string, slots []int) {
	t.Helper()
	list := "ok list"
	for _, s := range slots {
		list += fmt.Sprintf(" %02d", s)
	}
	startDaemon(t, sock, func(line string) string {
		if strings.HasPrefix(line, "version") {
			return "ok version " + stamp + "\n"
		}
		return list + "\n"
	})
}

func startRefusingDaemon(t *testing.T, sock string, class macagent.Class, message string) {
	t.Helper()
	startDaemon(t, sock, func(string) string { return "err " + string(class) + " " + message + "\n" })
}

func startDaemon(t *testing.T, sock string, answer func(line string) string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("binding the stand-in daemon on %s: %v", sock, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, macagent.MaxRequestBytes)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				_, _ = conn.Write([]byte(answer(string(buf[:n]))))
			}()
		}
	}()
}
