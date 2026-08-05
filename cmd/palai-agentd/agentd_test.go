package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/macagent"
)

// ---------------------------------------------------------------------------------------------------
// The fake privileged surface.
//
// IT IS NOT MORE GENEROUS THAN PRODUCTION, and that is load-bearing rather than tidy. This repository
// has already paid for both directions: a fake that mirrored production inherited production's bug, and
// a fake more generous than production made somebody write code against a shape that does not exist. So
// this one derives names with macagent.AccountName — the SAME function SysadminctlAccounts calls — which
// means it cannot accept a slot production would refuse, and cannot spell a name production would not.
// ---------------------------------------------------------------------------------------------------

type fakeAccounts struct {
	mu      sync.Mutex
	created map[int]string
	deleted []int
	spawned []int
	calls   int
}

func newFakeAccounts() *fakeAccounts { return &fakeAccounts{created: map[int]string{}} }

func (f *fakeAccounts) Create(_ context.Context, slot int) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	name, err := macagent.AccountName(slot)
	if err != nil {
		return "", "", err
	}
	home, err := macagent.HomeDir(slot)
	if err != nil {
		return "", "", err
	}
	if _, ok := f.created[slot]; ok {
		return "", "", macagent.Errorf(macagent.ClassExists, "%s already exists", name)
	}
	f.created[slot] = name
	return name, home, nil
}

// Spawn answers only for a slot this fake has CREATED, because that is the shape the real one has: a
// spawn is refused for a record that is not there. A fake more generous than production is how a caller
// ends up written against a shape that does not exist.
func (f *fakeAccounts) Spawn(_ context.Context, slot int) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	name, ok := f.created[slot]
	if !ok {
		derived, err := macagent.AccountName(slot)
		if err != nil {
			return "", 0, err
		}
		return "", 0, macagent.Errorf(macagent.ClassNotFound, "%s has no account", derived)
	}
	f.spawned = append(f.spawned, slot)
	return name, 10000 + slot, nil
}

func (f *fakeAccounts) Delete(_ context.Context, slot int) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	name, err := macagent.AccountName(slot)
	if err != nil {
		return "", "", err
	}
	home, err := macagent.HomeDir(slot)
	if err != nil {
		return "", "", err
	}
	if _, ok := f.created[slot]; !ok {
		return "", "", macagent.Errorf(macagent.ClassNotFound, "%s has no account", name)
	}
	delete(f.created, slot)
	f.deleted = append(f.deleted, slot)
	return name, home, nil
}

func (f *fakeAccounts) List(context.Context) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	slots := []int{}
	for slot := range f.created {
		slots = append(slots, slot)
	}
	return slots, nil
}

// openedAccounts is what the refusal tests assert on: the names that exist because of what was sent.
func (f *fakeAccounts) openedAccounts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := []string{}
	for _, n := range f.created {
		names = append(names, n)
	}
	return names
}

// ---------------------------------------------------------------------------------------------------
// A served daemon, on a socket the test OWNS the posture of.
//
// The condition under test is supplied here rather than inherited from the machine: this repository has
// already shipped four proofs that were green because of something the harness happened not to provide.
// So the socket is bound, chmod'ed and chgrp-checked against this process's own gid, which makes the
// test mean the same thing on a developer's Mac and on the Linux runner CI actually uses.
// ---------------------------------------------------------------------------------------------------

// socketDir returns a SHORT temporary directory. t.TempDir() on macOS lives under
// /var/folders/<hash>/<hash>/T/<TestName>/001 and a unix socket path is capped near 104 bytes, so a
// long-named test would fail to bind for a reason that has nothing to do with what it asserts.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agentd")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// listenAt binds a socket, puts it in the requested mode, and returns the gid it ACTUALLY has.
//
// The gid is measured rather than assumed, and finding that out cost a red run worth writing down: on
// macOS a node created in /tmp inherits the DIRECTORY's group — /private/tmp is root:wheel — so the
// socket came up gid 0 while the test expected the process's gid 20. BSD group inheritance, not a bug.
// Production does not have this problem because it sets the group explicitly (launchd's SockPathGroup,
// and the os.Chown in listen()), but a test that took its own gid on faith would have been asserting
// about a group nothing had set.
func listenAt(t *testing.T, path string, mode os.FileMode) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s to %04o: %v", path, mode, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("cannot read the group of %s on this platform", path)
	}
	if got := fi.Mode().Perm(); got != mode {
		t.Fatalf("asked for mode %04o on %s and got %04o, so this case is not testing what it names", mode, path, got)
	}
	return ln, int(st.Gid)
}

// serveFake starts a daemon on a well-postured socket and returns its path.
func serveFake(t *testing.T, accounts Accounts) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "a.sock")
	ln, gid := listenAt(t, path, socketMode)
	ctx, cancel := context.WithCancel(context.Background())
	srv := &Server{Accounts: accounts, SocketPath: path, WantGID: gid}
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() { done <- srv.Serve(ctx, ln); close(stopped) }()
	// Cleanup waits on `stopped` rather than on `done`, because `done` is drained below when Serve
	// returns early: a cleanup racing for the same value would hang for five seconds and report a
	// shutdown failure on top of whatever actually went wrong.
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("the daemon did not stop when its context was cancelled")
		}
	})
	// A refusal returns from Serve immediately; without this a posture bug would show up as a
	// confusing dial error in every test rather than as the refusal it is.
	select {
	case err := <-done:
		t.Fatalf("the daemon refused to serve a socket this test postured correctly: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	return path
}

// ask sends one request line and returns the reply line, or "" if the daemon sent nothing.
func ask(t *testing.T, path, line string) string {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, line); err != nil {
		return ""
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && reply == "" {
		return ""
	}
	return reply
}

// ---------------------------------------------------------------------------------------------------
// 1. A SLOT IS AN INTEGER, AND IT IS NOT SHELL TEXT.
// ---------------------------------------------------------------------------------------------------

// TestASlotThatIsNotATwoDigitNumberOpensNoAccount drives the socket — the surface a caller actually
// reaches — with every shape that is not a slot, and asserts two things about each: the daemon refuses
// with a class a caller can branch on, and NO ACCOUNT EXISTS afterwards.
//
// The second assertion is the one that matters. "It returned an error" and "it did not create an
// account" are different claims, and only the second is the security property: a daemon that answered
// `err` after creating palai-s00 would pass a test that only read the reply.
func TestASlotThatIsNotATwoDigitNumberOpensNoAccount(t *testing.T) {
	accounts := newFakeAccounts()
	path := serveFake(t, accounts)

	// Every one of these is a number some parser would accept, or a string that looks like an
	// argument. None of them is a slot.
	cases := []struct {
		line  string
		why   string
		class macagent.Class
	}{
		{"create 0\n", "one digit is not two", macagent.ClassBadSlot},
		{"create 00\n", "00 is two digits and is outside 01..99", macagent.ClassBadSlot},
		{"create 100\n", "three digits is not two", macagent.ClassBadSlot},
		{"create -1\n", "a sign is not a digit", macagent.ClassBadSlot},
		{"create 07; rm -rf /\n", "a shell fragment is extra tokens, not a slot", macagent.ClassBadRequest},
		{"create 0x11\n", "hex reaches 17 by a path nobody tested", macagent.ClassBadSlot},
		{"create +7\n", "a sign is not a digit", macagent.ClassBadSlot},
		{"create 007\n", "three digits is not two", macagent.ClassBadSlot},
		{"create  07\n", "two spaces is an empty token", macagent.ClassBadRequest},
		{"delete salih\n", "a name is not a slot, and this is the sentence the protocol exists to refuse", macagent.ClassBadSlot},
		{"delete 07 salih\n", "a second argument is not part of any request", macagent.ClassBadRequest},
		{"create\n", "a slot is not optional", macagent.ClassBadRequest},
		{"chown 07\n", "not a verb", macagent.ClassUnknownVerb},
		{"create " + strings.Repeat("7", 400) + "\n", "an over-long line is refused before it is parsed", macagent.ClassBadRequest},
	}

	for _, tc := range cases {
		reply := ask(t, path, tc.line)
		resp, err := macagent.ParseResponse(reply)
		if err != nil {
			t.Fatalf("%q: the daemon answered %q, which is not a response: %v", tc.line, reply, err)
		}
		if resp.OK {
			t.Errorf("%q: ACCEPTED (%s) — %s", tc.line, strings.TrimSpace(reply), tc.why)
			continue
		}
		if resp.Class != tc.class {
			t.Errorf("%q: refused with class %q, want %q (%s)", tc.line, resp.Class, tc.class, tc.why)
		}
	}

	if opened := accounts.openedAccounts(); len(opened) != 0 {
		t.Fatalf("no request above named a slot, yet these accounts exist: %v", opened)
	}

	// THE CONTROL. Without it this test would pass on a daemon that refuses everything, which is the
	// shape of assertion this repository keeps finding: green for a reason unrelated to the claim.
	reply := ask(t, path, "create 07\n")
	resp, err := macagent.ParseResponse(reply)
	if err != nil || !resp.OK {
		t.Fatalf("a well-formed request was refused, so the refusals above prove nothing: %q (%v)", reply, err)
	}
	if resp.Name != "palai-s07" || resp.Home != "/Users/palai-s07" {
		t.Fatalf("create 07 answered name %q home %q, want palai-s07 and /Users/palai-s07", resp.Name, resp.Home)
	}
	if opened := accounts.openedAccounts(); len(opened) != 1 || opened[0] != "palai-s07" {
		t.Fatalf("after one good create the accounts are %v, want exactly [palai-s07]", opened)
	}
}

// ---------------------------------------------------------------------------------------------------
// 2. THE CALLER CANNOT SEND A NAME — AND AN ABSENCE IS READ OUT OF THE AST, NOT OBSERVED.
// ---------------------------------------------------------------------------------------------------

// TestTheProtocolCannotExpressAnAccountNameAtAll asserts the DECLARATIONS, because that is the only
// place this property lives.
//
// There is no behaviour to observe here: you cannot send a request that carries a name and watch it be
// refused, precisely because there is nowhere in a request to put one. The only way to regress it is to
// change a signature — add a `Name string` to Request, or a `name string` parameter to Accounts — so
// the signature is what this test reads. The same shape was used for holdMachine in this tree and it
// was right there too.
func TestTheProtocolCannotExpressAnAccountNameAtAll(t *testing.T) {
	protoTypes := declaredTypes(t, filepath.Join("..", "..", "packages", "macagent"))

	req, ok := protoTypes["Request"].(*ast.StructType)
	if !ok {
		t.Fatalf("macagent.Request is not a struct; it is %T", protoTypes["Request"])
	}
	fields := 0
	for _, f := range req.Fields.List {
		names := fieldNames(f)
		for _, name := range names {
			fields++
			kind := resolveBuiltin(protoTypes, f.Type, 0)
			if kind == "" {
				t.Errorf("Request.%s has type %s, which does not resolve to an integer builtin — a request field that can hold arbitrary text is a request that can carry an account name",
					name, exprString(f.Type))
				continue
			}
			if strings.Contains(kind, "string") {
				t.Errorf("Request.%s resolves to %s. THIS IS THE HOLE THE TYPE EXISTS TO CLOSE: a string field in a request lets a compromised caller say `delete salih`.",
					name, kind)
			}
		}
	}
	if fields != 2 {
		t.Errorf("Request has %d fields, want exactly 2 (Verb and Slot); a new field is a new thing a caller can choose", fields)
	}

	// And the other half: the privileged surface itself takes integers. A protocol that cannot express
	// a name is worth nothing if the interface behind it accepts one.
	daemonTypes := declaredTypes(t, ".")
	iface, ok := daemonTypes["Accounts"].(*ast.InterfaceType)
	if !ok {
		t.Fatalf("Accounts is not an interface; it is %T", daemonTypes["Accounts"])
	}
	methods := 0
	for _, m := range iface.Methods.List {
		fn, ok := m.Type.(*ast.FuncType)
		if !ok || len(m.Names) == 0 {
			continue
		}
		methods++
		for _, p := range fn.Params.List {
			got := exprString(p.Type)
			if got == "context.Context" || got == "int" {
				continue
			}
			t.Errorf("Accounts.%s takes a parameter of type %s. Every parameter must be a context or an int: a name reaching this interface is a name the daemon did not derive.",
				m.Names[0].Name, got)
		}
	}
	if methods != 4 {
		t.Errorf("Accounts has %d methods, want exactly 4 (Create, Delete, List, Spawn); the verb set is the privilege", methods)
	}

	// The claim is only as strong as the parse. If either lookup silently found nothing, everything
	// above is vacuously green — which is the failure mode this repository names most often.
	if len(protoTypes) == 0 || len(daemonTypes) == 0 {
		t.Fatalf("parsed %d protocol types and %d daemon types; a scan that found nothing asserts nothing",
			len(protoTypes), len(daemonTypes))
	}
}

// declaredTypes maps every type name declared in the non-test files of a directory to its definition.
func declaredTypes(t *testing.T, dir string) map[string]ast.Expr {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	out := map[string]ast.Expr{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						out[ts.Name.Name] = ts.Type
					}
				}
			}
		}
	}
	return out
}

func fieldNames(f *ast.Field) []string {
	if len(f.Names) == 0 {
		return []string{exprString(f.Type)} // an embedded field
	}
	names := make([]string, 0, len(f.Names))
	for _, n := range f.Names {
		names = append(names, n.Name)
	}
	return names
}

// resolveBuiltin follows named types down to a builtin and returns it, or "" if it is not an integer
// builtin. A named type whose chain ends in `string` returns that string so the failure can say so.
func resolveBuiltin(types map[string]ast.Expr, expr ast.Expr, depth int) string {
	if depth > 8 {
		return ""
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return "" // a slice, map, pointer, selector or struct literal is not an integer builtin
	}
	switch ident.Name {
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "byte", "rune":
		return ident.Name
	case "string":
		return "string"
	}
	next, ok := types[ident.Name]
	if !ok {
		return ""
	}
	return resolveBuiltin(types, next, depth+1)
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	case *ast.ArrayType:
		return "[]" + exprString(v.Elt)
	case *ast.MapType:
		return "map[" + exprString(v.Key) + "]" + exprString(v.Value)
	default:
		return fmt.Sprintf("%T", e)
	}
}

// ---------------------------------------------------------------------------------------------------
// 3. A PERMISSION THAT PROTECTS ROOT IS CHECKED, NOT ASSUMED.
// ---------------------------------------------------------------------------------------------------

// TestADaemonRefusesToServeAnOverPermissiveSocket asserts that the daemon inspects the posture it was
// handed and declines to answer on one nobody chose.
//
// This matters because in production the socket is created by a plist this process did not write. The
// mode and the group ARE the credential — there is no password, no token and no TTY between a caller
// and root here — so a daemon that served whatever it was given would be a daemon whose whole
// authorisation story is a file somebody hopes is right.
//
// Both directions are asserted. A refusal test that refuses everything proves nothing, so the last case
// is a socket postured correctly, and it must serve.
func TestADaemonRefusesToServeAnOverPermissiveSocket(t *testing.T) {
	cases := []struct {
		name string
		mode os.FileMode
		// wrongGroup configures the daemon for a group the socket does NOT belong to. It is a flag
		// rather than a number because the socket's real gid is whatever the platform gave it, and a
		// hard-coded expectation is the thing that made this test red the first time.
		wrongGroup bool
		serves     bool
		mustSay    string
	}{
		{"world readable and writable", 0o666, false, false, "mode 0666"},
		{"world readable", 0o664, false, false, "mode 0664"},
		{"group executable", 0o770, false, false, "mode 0770"},
		{"nobody can reach it", 0o600, false, false, "mode 0600"},
		{"the wrong group", 0o660, true, false, "gid"},
		{"exactly 0660 and the right group", 0o660, false, true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "a.sock")
			ln, gid := listenAt(t, path, tc.mode)
			if tc.wrongGroup {
				gid++
			}
			accounts := newFakeAccounts()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := &Server{Accounts: accounts, SocketPath: path, WantGID: gid}
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ctx, ln) }()

			if tc.serves {
				if reply := ask(t, path, "list\n"); !strings.HasPrefix(reply, "ok list") {
					t.Fatalf("a correctly postured socket answered %q; the refusals in this test would prove nothing if the daemon simply never serves", reply)
				}
				cancel()
				<-done
				return
			}

			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the daemon neither refused nor returned; it is serving a socket it should have declined")
			}
			if err == nil {
				t.Fatal("the daemon returned no error, so it accepted this posture")
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("refusal %q does not name %q, so an operator cannot tell what to fix", err, tc.mustSay)
			}

			// REFUSING IS NOT ENOUGH: it must also not have answered anybody. A daemon that logged a
			// complaint and served on is the failure this test exists to catch.
			if reply := ask(t, path, "create 07\n"); reply != "" {
				t.Errorf("the daemon refused the posture and then answered %q on that same socket", reply)
			}
			if accounts.calls != 0 {
				t.Errorf("the daemon refused the posture and still made %d privileged calls", accounts.calls)
			}
		})
	}
}

// TestTheSocketCheckRefusesAPathThatIsNotASocket covers the other way requireSocketPosture can be
// pointed somewhere unintended: a regular file, or nothing at all.
func TestTheSocketCheckRefusesAPathThatIsNotASocket(t *testing.T) {
	dir := socketDir(t)
	regular := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(regular, nil, 0o660); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := requireSocketPosture(regular, socketMode, os.Getgid()); err == nil {
		t.Error("a regular file with the right mode was accepted as a socket")
	}
	if err := requireSocketPosture(filepath.Join(dir, "absent"), socketMode, os.Getgid()); err == nil {
		t.Error("a path that does not exist was accepted")
	}
}

// TestAnErrorWithoutAClassIsInternal pins the mapping a caller depends on: it branches on the class, so
// every refusal has to carry one, and an unclassified failure must not silently become a class that
// means something else.
func TestAnErrorWithoutAClassIsInternal(t *testing.T) {
	if got := responseFor(errors.New("boom")).Class; got != macagent.ClassInternal {
		t.Errorf("an unclassified error became class %q, want %q", got, macagent.ClassInternal)
	}
	if got := responseFor(macagent.Errorf(macagent.ClassRefused, "no")).Class; got != macagent.ClassRefused {
		t.Errorf("a classed error became %q, want %q", got, macagent.ClassRefused)
	}
	if got := responseFor(fmt.Errorf("wrapped: %w", macagent.Errorf(macagent.ClassNotFound, "no"))).Class; got != macagent.ClassNotFound {
		t.Errorf("a wrapped classed error became %q, want %q", got, macagent.ClassNotFound)
	}
}
