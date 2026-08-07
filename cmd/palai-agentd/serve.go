package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/palgroup/palai/packages/macagent"
)

// socketMode is the only mode this daemon will serve on: owner and group read/write, world nothing.
//
// Group membership IS the authorisation. There is no password, no token and no TTY between a caller and
// root here — being in the `palai` group is the whole credential — so the mode and the group are not
// cosmetics on top of a check, they ARE the check.
const socketMode os.FileMode = 0o660

// connDeadline bounds one request. A client that connects and then says nothing must not hold a
// goroutine on a root daemon open forever.
const connDeadline = 10 * time.Second

// accountConnDeadline bounds a request that does real work in the OPERATING SYSTEM rather than answering
// from memory. It mirrors macagent.AccountExchangeTimeout on the client, and the two are kept equal on
// purpose: bounds that disagree are a connection one side closes while the other is still waiting.
const accountConnDeadline = 90 * time.Second

// Server answers requests on a unix socket.
type Server struct {
	Accounts   Accounts
	SocketPath string
	// AllocationRoot is the directory this machine opens session workspaces under — the SAME value the
	// agent was enrolled with. VerbAdopt joins a slot onto it, which is how a path never crosses the
	// wire: the caller chooses an integer and this daemon chooses the directory.
	//
	// Empty means this daemon can adopt nothing and says so, rather than guessing a root and handing a
	// tenant a directory nobody meant.
	AllocationRoot string
	// WantGID is the gid the socket must belong to. It is a gid rather than a group name because that
	// is what a stat returns and what launchd's SockPathGroup takes; resolving the name happens once,
	// at startup, where a missing group is a refusal to start rather than a per-request surprise.
	WantGID int
	Logger  *log.Logger
	// Version is this daemon's build stamp, answered on the wire so a caller compares against what is
	// RUNNING rather than against a file on disk. Empty is a daemon that was wired without one, and it
	// answers an empty stamp rather than refusing: a version is not a credential and no verb depends on
	// it.
	Version string
}

// Serve verifies the socket, then answers on it until ctx is done.
//
// ‼️ THE POSTURE IS CHECKED, NOT ASSUMED, AND IT IS CHECKED HERE RATHER THAN WHERE THE SOCKET IS MADE.
// This daemon is meant to be started by launchd with the listener handed to it, so in production the
// mode and group are set by a plist this process did not write and cannot see. A permission that
// protects a secret is not assumed without being checked — and the same check is worth running when the
// daemon binds its own socket, because a chmod can lose a race with a connect, an operator can leave a
// stale node behind, and a umask is not a thing this process controls.
//
// It refuses by CLOSING the listener and returning, so a caller reaching an unsafe socket gets a closed
// connection rather than a hang.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if err := requireSocketPosture(s.SocketPath, socketMode, s.WantGID); err != nil {
		_ = ln.Close()
		return err
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			return err
		}
		// Serially, one request per connection. This daemon exists to create and delete accounts —
		// operations measured in seconds that must not interleave — and a slot allocated twice
		// concurrently is the exact collision the accounts are here to prevent.
		s.handle(ctx, conn)
	}
}

// requireSocketPosture refuses any socket that is not exactly mode 0660 and group wantGID.
//
// It refuses on an exact mode rather than on "no world bits", because the interesting failure is not
// only 0666: a 0600 socket nothing can reach and a 0770 one that grants execute are both postures
// nobody chose, and a daemon that silently served them would be a daemon whose plist nobody has to get
// right.
func requireSocketPosture(path string, wantMode os.FileMode, wantGID int) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("refusing to serve: cannot stat the socket %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to serve: %s is not a socket (mode %s)", path, fi.Mode())
	}
	if got := fi.Mode().Perm(); got != wantMode {
		return fmt.Errorf("refusing to serve: socket %s is mode %04o, want %04o — group membership is the only credential this daemon has, so a mode nobody chose is a mode nobody reviewed",
			path, got, wantMode)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("refusing to serve: cannot read the owner of %s on this platform", path)
	}
	if int(st.Gid) != wantGID {
		return fmt.Errorf("refusing to serve: socket %s belongs to gid %d, want gid %d", path, st.Gid, wantGID)
	}
	return nil
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connDeadline))

	// Bounded before it is parsed. MaxRequestBytes+1 lets an over-long line be DETECTED rather than
	// silently truncated into a shorter request that happens to be well formed.
	r := bufio.NewReader(io.LimitReader(conn, macagent.MaxRequestBytes+1))
	line, err := r.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		s.write(conn, macagent.Err(macagent.ClassBadRequest, "could not read a request line: "+err.Error()))
		return
	}
	// ‼️ THE VERB DECIDES THE DEADLINE, and this is the SERVER half of a bound the client already got.
	// `sysadminctl -addUser` writes a user record and a home directory; on a busy Mac that is seconds. A
	// ten-second connection deadline closed the socket mid-create and the caller read EOF — measured on
	// a real Mac on 2026-08-07, on the SECOND session, because the first found its account already there
	// and never took this path.
	//
	// Raising it for everything would give back the protection it exists for: a client that connects and
	// says nothing still gets ten seconds, because version and list answer from memory and a slow answer
	// from either IS the wedge that bound is for.
	if req, err := macagent.ParseRequest(line); err == nil && (req.Verb == macagent.VerbCreate ||
		req.Verb == macagent.VerbDelete || req.Verb == macagent.VerbAdopt) {
		_ = conn.SetDeadline(time.Now().Add(accountConnDeadline))
	}
	s.write(conn, s.answer(ctx, line))
}

// answer turns one request line into one response. It is separate from the connection so the dispatch
// can be tested without a socket, and it never returns a bare error: every refusal carries the class a
// caller branches on.
func (s *Server) answer(ctx context.Context, line string) macagent.Response {
	req, err := macagent.ParseRequest(line)
	if err != nil {
		return responseFor(err)
	}
	switch req.Verb {
	case macagent.VerbCreate:
		name, home, err := s.Accounts.Create(ctx, req.Slot)
		if err != nil {
			return responseFor(err)
		}
		return macagent.OKAccount(macagent.VerbCreate, name, home)
	case macagent.VerbDelete:
		name, home, err := s.Accounts.Delete(ctx, req.Slot)
		if err != nil {
			return responseFor(err)
		}
		return macagent.OKAccount(macagent.VerbDelete, name, home)
	case macagent.VerbList:
		slots, err := s.Accounts.List(ctx)
		if err != nil {
			return responseFor(err)
		}
		return macagent.OKList(slots)
	case macagent.VerbAdopt:
		return s.adopt(ctx, req.Slot)
	case macagent.VerbSpawn:
		name, pid, err := s.Accounts.Spawn(ctx, req.Slot)
		if err != nil {
			return responseFor(err)
		}
		return macagent.OKSpawn(name, pid)
	case macagent.VerbVersion:
		// It touches no account and takes no argument, so it is answered without consulting Accounts at
		// all — including on a machine that is not a Mac, where every other verb is `unsupported`. A
		// caller asking "what is running here" deserves an answer from a daemon that cannot do anything
		// else.
		return macagent.OKVersion(s.Version)
	}
	// Unreachable while ParseRequest only ever produces the verbs above, and a response rather
	// than a panic because a root daemon that crashes on an unexpected value is a denial of service
	// anyone in the group can trigger.
	return macagent.Err(macagent.ClassUnknownVerb, "verb "+req.Verb.String()+" has no handler")
}

// responseFor maps an error onto a classed response. An error that carries no class is internal, which
// is the safe default: a caller sees "something failed here" rather than a class it might branch on.
func responseFor(err error) macagent.Response {
	var e *macagent.Error
	if errors.As(err, &e) {
		return macagent.Err(e.Class, e.Message)
	}
	return macagent.Err(macagent.ClassInternal, err.Error())
}

func (s *Server) write(conn net.Conn, resp macagent.Response) {
	if _, err := io.WriteString(conn, resp.Line()); err != nil && s.Logger != nil {
		s.Logger.Printf("writing a response failed: %v", err)
	}
}
