// Command palai-agentd owns session accounts on a Mac.
//
// WHY IT EXISTS. A session on a Mac is isolated by its uid and by nothing else — `xcrun simctl spawn`
// defeats every weaker mechanism on this platform — so a session needs an account created when it
// starts and destroyed when it ends. Creating one needs root, and the control plane is not root.
//
// WHY A DAEMON RATHER THAN sudo. There are a hundred of these machines and nobody is going to type a
// password on them. `sudo` is the wrong tool three ways: it needs a password, it silently rewrites the
// environment (secure_path once made the same command run BSD `find` under sudo and `bfs` in a shell in
// this repository, opening a three-hour window in an ISO8601 stamp), and a sudoers line grants
// everything the named binary can be argued into doing. A root LaunchDaemon behind a group-owned unix
// socket closes all three: no password, no TTY, no inherited environment, and a verb set that is five
// words long.
//
// WHY IT ALSO STARTS PROCESSES. An account nothing runs under is a directory, not a boundary — and
// nothing could run under one, because dropping to another uid needs uid 0 and no other Palai process
// has it. So `spawn` starts a session worker AS `palai-sNN`, and that is the point at which a tenant's
// work stops being the operator's own uid. It is the one verb that runs a program and it is still only
// an integer wide: the caller says which slot, the daemon says which program
// (macagent.InstalledWorkerPath, a constant).
//
// WHAT IT WILL NOT DO. It takes a slot number. It does not take a name, a path, or a flag, and it never
// runs a shell. See packages/macagent for why that is a property of the type rather than a check.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"syscall"

	"github.com/palgroup/palai/packages/macagent"
	"github.com/palgroup/palai/packages/version"
)

// defaultSocketPath is where the plist in deploy/macos puts the socket, and defaultGroup is the group
// whose members may reach this daemon — membership in it is the entire credential.
//
// BOTH NOW COME FROM packages/macagent RATHER THAN BEING WRITTEN HERE, because Task 2 gave them a second
// reader: every caller dials the same path and the installer puts the same account in the same group. A
// path written twice is a path an install can land beside rather than on.
const (
	defaultSocketPath = macagent.DefaultSocketPath
	defaultGroup      = macagent.DefaultGroup
)

func main() {
	socketPath := flag.String("socket", defaultSocketPath, "unix socket to serve on")
	groupName := flag.String("group", defaultGroup, "group that owns the socket; membership in it is the credential")
	showVersion := flag.Bool("version", false, "print the daemon version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Resolve())
		return
	}

	logger := log.New(os.Stderr, "palai-agentd: ", log.LstdFlags|log.LUTC)
	if err := run(context.Background(), *socketPath, *groupName, logger); err != nil {
		logger.Printf("%v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, socketPath, groupName string, logger *log.Logger) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("palai-agentd manages macOS session accounts and this is %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("palai-agentd creates and deletes accounts, which needs root; it is running as uid %d", os.Geteuid())
	}
	gid, err := lookupGID(groupName)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := listen(socketPath, gid)
	if err != nil {
		return err
	}

	srv := &Server{
		Accounts:   NewSysadminctlAccounts(),
		SocketPath: socketPath,
		WantGID:    gid,
		Logger:     logger,
		// The stamp of the daemon that is ACTUALLY SERVING, which is the only version an upgrade
		// decision may be made on: a copy that landed at InstalledBinaryPath and a launchd job that was
		// never restarted disagree, and the file is the one that lies.
		Version: version.Resolve(),
	}
	logger.Printf("serving %s for group %s (gid %d)", socketPath, groupName, gid)
	if err := srv.Serve(ctx, ln); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// lookupGID resolves the group whose members may reach this daemon.
//
// A MISSING GROUP IS A REFUSAL TO START, not a fallback. Falling back to any other gid would widen the
// credential silently, and "the daemon came up" would stop meaning "the socket is reachable by exactly
// the accounts the installer put in that group".
func lookupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("group %q does not exist, so nothing could be authorised to reach this daemon: %w", name, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("group %q has a non-numeric gid %q: %w", name, g.Gid, err)
	}
	return gid, nil
}

// listen binds the socket and puts it in the posture Serve will insist on.
//
// ‼️ THIS IS NOT launchd SOCKET ACTIVATION, AND THE DIFFERENCE IS WORTH STATING RATHER THAN GLOSSING.
// Handing a listener to a job on demand is done with `launch_activate_socket`, a C function in
// libSystem with no wrapper in the standard library or in golang.org/x/sys (checked: zero hits across
// every x/sys in the module cache). Calling it needs cgo, and this tree has zero cgo — every release
// build is CGO_ENABLED=0, and CI is ubuntu-24.04, so a cgo daemon would be a daemon CI never compiles.
//
// So the plist uses RunAtLoad with KeepAlive and the daemon binds its own socket. What is kept: launchd
// starts it at boot, restarts it when it dies, and "is it up?" stops being a question anyone asks. What
// is lost: the socket does not exist until the daemon has bound it, so a caller racing a cold boot gets
// a connection refused rather than a queued connection. Task 2 probes the socket before enrolling a
// machine, so that race has a place to be handled — but it is a real difference from the plan and it is
// named here rather than discovered later.
func listen(socketPath string, gid int) (net.Listener, error) {
	// A stale node from a killed daemon would make bind fail with "address already in use". It is only
	// ever removed if it is a socket at exactly the configured path — a regular file or a directory
	// there is somebody else's, and a root daemon that unlinks whatever it finds is a root daemon that
	// can be aimed.
	if fi, err := os.Lstat(socketPath); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket (mode %s); refusing to remove it", socketPath, fi.Mode())
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("removing the stale socket %s: %w", socketPath, err)
		}
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("binding %s: %w", socketPath, err)
	}
	if err := os.Chown(socketPath, 0, gid); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("setting the group of %s to gid %d: %w", socketPath, gid, err)
	}
	// Set AFTER bind, because the umask applies to the node the bind creates and this process does not
	// own the umask it inherited. Serve stats the result rather than trusting this line.
	if err := os.Chmod(socketPath, socketMode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("setting the mode of %s to %04o: %w", socketPath, socketMode, err)
	}
	return ln, nil
}
