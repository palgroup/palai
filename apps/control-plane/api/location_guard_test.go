package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// The dangling-Location guard (E29 T2).
//
// A 201's Location header is the one thing that tells a caller "the resource you just created lives
// HERE". router.go:113-114 already writes the failure this guard exists to prevent, in the tree's own
// words: "a prefix no epic ever mounted, so following the header 404'd". That sentence was written
// about ONE address, the address was fixed, and the CLASS was never swept — on 2026-07-31 seven
// Location prefixes still answered Go's bare "404 page not found".
//
// The invariant is TOTAL and there is deliberately NO exception list: a Location either resolves, or
// it is not written. There is no name to add here to make a violation go away, because the two honest
// closures are "mount the route" and "delete the header" and both of them make this guard pass on
// their own. The header's ABSENCE is honest; its presence pointing nowhere is a lie. secret_refs.go:99
// is the shipped precedent — a resource addressed by name writes no Location and says why.
//
// RFC 9110 (June 2022) grounds BOTH halves of that, verbatim (fetched 2026-07-31 from
// https://www.rfc-editor.org/rfc/rfc9110.txt):
//
//	§10.2.2: "For 201 (Created) responses, the Location value refers to the primary resource created
//	          by the request."
//	§15.3.2: "The primary resource created by the request is identified by either a Location header
//	          field in the response or, if no Location header field is received, by the target URI."
//
// The first is why a Location naming an unserved address is wrong rather than merely untidy: the field
// is defined to REFER to the created resource, and a reference into a 404 refers to nothing. The second
// is why deleting the header is a legitimate closure and not a loss of contract — the no-Location case
// is specified, and identification falls back to the target URI. Removing a dangling header moves a
// response from "wrong" to "defined", and the minted id stays in the 201 body either way.
//
// TWO DIRECTIONS, because a sweep only ever looks in one and there is a different bug in each.
//
//   - RESOLUTION (TestEveryLocationHeaderResolvesToAMountedRoute): producers -> mounts. For every
//     Location this package writes, does the SHIPPED router serve that address? This finds a header
//     pointing nowhere. It cannot find a producer it never visited.
//   - CONTAINMENT (TestNoLocationHeaderIsWrittenOutsideTheScannedPackage): the scanned region -> the
//     repository. Is every Location producer in the tree inside the region the resolution leg walks?
//     This finds the producer the first leg cannot see. Without it the first leg is scoped to one
//     directory and reads as total — green because it did not look.
//
// The direction NOT taken, on purpose: "every 201 must carry a Location". That would be a different
// and WRONG invariant — §2 permits a create to write no header at all, and secret_refs.go relies on it.
// This guard constrains headers that ARE written; it never demands one.
//
// The mount side is a PROBE of the router NewRouter actually returns, not a read of router.go's source.
// A route table is a string until something dispatches on it, and this repository has shipped guards
// that were green because they read a string that looked right. The probe reproduces exactly what a
// client following the header experiences.

// probeSegment stands in for any path segment a handler computes at run time (a minted id). It is a
// legal single segment, so it matches a {wildcard} and matches nothing else.
const probeSegment = "e29probe"

// muxMiss is the exact body net/http.NotFoundHandler writes when a ServeMux has no pattern for a path.
// Every handler in this tree answers with an RFC 9457 problem+json instead, so the two are never
// confused; this string is what "following the header 404'd" means, mechanically.
const muxMiss = "404 page not found"

// locationProducer is one resolved Location write: where it is, and the concrete address a client
// following it would request.
type locationProducer struct {
	where string // file:line of the Header().Set that writes the header
	via   string // file:line of the call site that supplied the prefix; "" when the write is direct
	addr  string // the address to follow, run-time segments replaced by probeSegment
}

// TestEveryLocationHeaderResolvesToAMountedRoute is the resolution leg. It collects every Location this
// package writes by parsing the package (not by grepping it — a text search cannot tell the NESTED
// prefix "/v1/tools/"+id+"/revisions/" apart from the bare "/v1/tools/", and those two addresses are
// mounted by different routes), then asks the shipped router to route each one.
func TestEveryLocationHeaderResolvesToAMountedRoute(t *testing.T) {
	producers := collectLocationProducers(t, ".")
	if len(producers) == 0 {
		t.Fatal("no Location producers found: the parser stopped matching the tree's shape, which makes this guard vacuous")
	}

	router := everyRouteMountedRouter(t)

	var dangling []string
	for _, p := range producers {
		if routerServes(router, p.addr) {
			continue
		}
		line := "  " + p.addr + "\n      written at " + p.where
		if p.via != "" {
			line += "\n      prefix from " + p.via
		}
		dangling = append(dangling, line)
	}
	if len(dangling) > 0 {
		sort.Strings(dangling)
		t.Fatalf("%d Location header(s) name an address the router does not serve; a client following one gets %q.\n"+
			"Close each ONE of two ways — mount the route, or delete the header. There is no third option and no exception list.\n"+
			"(If the address IS mounted, everyRouteMountedRouter below is missing the surface that mounts it: the probe is only\n"+
			"as complete as that fixture, so add the wiring there rather than exempting the address.)\n%s",
			len(dangling), muxMiss, strings.Join(dangling, "\n"))
	}
	t.Logf("%d Location producers, every one resolving against the shipped router", len(producers))
}

// TestNoLocationHeaderIsWrittenOutsideTheScannedPackage is the containment leg. The resolution leg walks
// apps/control-plane/api; a Location written in any other package is invisible to it and would ship
// unchecked. Today there are none, and this asserts that rather than assuming it.
func TestNoLocationHeaderIsWrittenOutsideTheScannedPackage(t *testing.T) {
	root := repoRootFromTest(t)
	scanned, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	var outside []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", ".next", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// containment by cleaned path prefix + a separator, so a sibling directory whose name merely
		// begins with the scanned one is not swallowed.
		if dir := filepath.Dir(path); dir == scanned || strings.HasPrefix(dir, scanned+string(filepath.Separator)) {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil // not our business to fail on unparseable generated files
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isHeaderMutation(call) || len(call.Args) == 0 {
				return true
			}
			if !isLocationLiteral(call.Args[0]) {
				return true
			}
			rel, _ := filepath.Rel(root, path)
			outside = append(outside, rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(outside) > 0 {
		sort.Strings(outside)
		t.Fatalf("Location header(s) written outside apps/control-plane/api, where the resolution leg cannot see them:\n  %s\n"+
			"Either move the write into the scanned package or widen collectLocationProducers to walk this one.",
			strings.Join(outside, "\n  "))
	}
}

// ---- the mount side: a probe of the shipped router -------------------------------------------------

// routerServes reports whether the router has a route for addr. The signal is Go's OWN ServeMux miss
// rather than any status this tree chooses: a handler that runs may answer 404 for a hundred reasons,
// but only an unrouted path produces net/http's plain-text not-found.
//
// It drives the handler directly rather than over a socket, so the only two outcomes are "the mux
// dispatched" and "the mux did not". A panic counts as SERVED — it can only happen after dispatch, and
// dispatch is the only question asked here — and it is caught explicitly rather than being folded in
// with transport errors, because a transport error is the guard failing to ask, not an answer.
func routerServes(h http.Handler, addr string) bool {
	// An address a client cannot even form is not one it can follow. This is the fail-closed answer for a
	// Location assembled at run time (fmt.Sprintf and friends), where the parser has no literal to read
	// and renders the whole thing as a probe segment.
	if !strings.HasPrefix(addr, "/") {
		return false
	}
	if _, err := url.Parse(addr); err != nil {
		return false
	}
	// httptest.NewRequest panics on a target it cannot turn into a request. That is still "a client could
	// not follow this", so it is resolved to false HERE rather than inside the dispatch recover below,
	// where a panic means the opposite.
	req, ok := func() (r *http.Request, ok bool) {
		defer func() {
			if recover() != nil {
				r, ok = nil, false
			}
		}()
		return httptest.NewRequest("GET", addr, nil), true
	}()
	if !ok {
		return false
	}
	req.Header.Set("Authorization", "Bearer probe")
	rec := httptest.NewRecorder()
	dispatched := func() (panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		h.ServeHTTP(rec, req)
		return false
	}()
	if dispatched {
		return true
	}
	if rec.Code != http.StatusNotFound {
		return true
	}
	return !strings.Contains(rec.Body.String(), muxMiss)
}

// everyRouteMountedRouter builds the router with EVERY optional surface wired. That is the deliberate
// best case: a deployment mounts a subset, so an address that does not resolve against the maximal
// router resolves in no deployment at all. A dangling header found here is dangling everywhere.
//
// It is a different fixture from capabilities_tier_test.go's fullyMountedRouter, which mounts the
// surfaces that ADVERTISE capabilities; this one mounts the surfaces that carry ROUTES.
func everyRouteMountedRouter(t *testing.T) http.Handler {
	t.Helper()
	nop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return NewRouter(
		fakeVerifier{},
		probeAdmitter{},
		probeEvents{},
		probeSessions{},
		probeBindings{},
		&fakeAgentRegistry{},
		&fakeWebhookAPI{},
		&fakeTriggerAPI{},
		&fakeScheduleAPI{},
		&fakeToolRegistry{},
		&fakeMCPRegistry{},
		&fakeSkillRegistry{},
		&fakeHookRegistry{},
		&fakeProvisioning{},
		&fakeArtifactAPI{},
		SSEConfig{},
		nop,
		nop,
		WithSecretRefs(&fakeSecretRefs{}),
		WithEnvironments(probeEnvironments{}),
		WithUsage(&fakeUsage{}),
		WithModelRoutes(&fakeModelRoutes{}),
		WithKnowledge(stubKnowledge{}),
		WithCapabilityWorkers(),
		WithQueueConnections(&fakeQueueAPI{}, publicResolver{}),
		WithSlackConnections(&fakeSlackConnectionAPI{}),
		WithRunners(&fakeRunnerRegistry{}),
		WithApprovals(&fakeApprovalAPI{}),
		WithBots(&fakeBotRegistry{}),
	)
}

// The four seams with no fake in this package yet. They exist so the routes MOUNT; the probe never
// reads what they return.
type probeAdmitter struct{}

func (probeAdmitter) AdmitResponse(context.Context, AdmitRequest) (AdmitResult, error) {
	return AdmitResult{}, nil
}
func (probeAdmitter) GetResponse(context.Context, middleware.Scope, string) (RetrieveResult, error) {
	return RetrieveResult{}, nil
}
func (probeAdmitter) ListResponses(context.Context, middleware.Scope, ListQuery) ([]ListRow, error) {
	return nil, nil
}
func (probeAdmitter) CancelResponse(context.Context, middleware.Scope, string) (RetrieveResult, error) {
	return RetrieveResult{}, nil
}
func (probeAdmitter) ResolveOrganization(context.Context, string) (string, error) {
	return "org_1", nil
}

type probeEvents struct{}

func (probeEvents) SessionExists(context.Context, string, string) (bool, error) {
	return false, nil
}
func (probeEvents) ResolveCursor(context.Context, string, string, string) (int64, bool, error) {
	return 0, false, nil
}
func (probeEvents) After(context.Context, string, string, int64, int) ([]contracts.Event, error) {
	return nil, nil
}
func (probeEvents) RecordAttachDenied(context.Context, string, string, string) error {
	return nil
}

type probeSessions struct{}

func (probeSessions) CreateSession(context.Context, middleware.Scope, string) (SessionResult, error) {
	return SessionResult{}, nil
}
func (probeSessions) RenameSession(context.Context, middleware.Scope, string, string) (SessionResult, error) {
	return SessionResult{}, nil
}
func (probeSessions) SetSessionAutoApprove(context.Context, middleware.Scope, string, bool, bool) (SessionResult, error) {
	return SessionResult{}, nil
}
func (probeSessions) GetSession(context.Context, middleware.Scope, string) (SessionResult, error) {
	return SessionResult{}, nil
}
func (probeSessions) ListSessions(context.Context, middleware.Scope, ListQuery) ([]ListRow, error) {
	return nil, nil
}
func (probeSessions) AcceptCommand(context.Context, middleware.Scope, string, contracts.CommandCreateRequest) (CommandResult, error) {
	return CommandResult{}, nil
}

type probeBindings struct{}

func (probeBindings) CreateRepositoryBinding(context.Context, middleware.Scope, RepositoryBindingCreate) (BindingResult, error) {
	return BindingResult{}, nil
}
func (probeBindings) GetRepositoryBinding(context.Context, middleware.Scope, string) (BindingResult, error) {
	return BindingResult{}, nil
}
func (probeBindings) ListRepositoryBindings(context.Context, middleware.Scope, ListQuery, bool) ([]ListRow, error) {
	return nil, nil
}

// The E30 lifecycle half. This probe answers "nothing changed" for all three, which is what this guard
// wants: it walks the router asserting every 201 carries a Location header, and a write that reports no
// change takes the 404 arm and never reaches that assertion.
func (probeBindings) SetRepositoryBindingConnection(context.Context, middleware.Scope, string, string) (bool, error) {
	return false, nil
}
func (probeBindings) ArchiveRepositoryBinding(context.Context, middleware.Scope, string) (bool, error) {
	return false, nil
}
func (probeBindings) UnarchiveRepositoryBinding(context.Context, middleware.Scope, string) (bool, error) {
	return false, nil
}

type probeEnvironments struct{}

func (probeEnvironments) CreateEnvironment(context.Context, middleware.Scope, []byte) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (probeEnvironments) ListEnvironments(context.Context, middleware.Scope) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (probeEnvironments) GetEnvironment(context.Context, middleware.Scope, string) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (probeEnvironments) PutEnvironmentValue(context.Context, middleware.Scope, string, []byte) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}
func (probeEnvironments) DeleteEnvironmentValue(context.Context, middleware.Scope, string, string) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}

// ---- the producer side: parsing, not grepping ------------------------------------------------------

// collectLocationProducers parses every non-test file in dir and resolves each Location write to a
// concrete address. It FAILS CLOSED: any mention of the Location header it cannot account for, and any
// expression shape it does not understand, is a test failure rather than a silent skip. A guard that
// quietly ignores what it cannot parse is the "green because it did not look" failure with extra steps.
func collectLocationProducers(t *testing.T, dir string) []locationProducer {
	t.Helper()
	fset := token.NewFileSet()
	pkgFiles := map[string]*ast.File{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		pkgFiles[name] = f
	}

	consts := packageStringConsts(pkgFiles)
	pos := func(n ast.Node) string {
		p := fset.Position(n.Pos())
		return filepath.Base(p.Filename) + ":" + strconv.Itoa(p.Line)
	}

	// Every string literal naming the Location header must be ACCOUNTED for: consumed by a recognised
	// producer shape, or explicitly a non-producer. Anything left over fails the guard.
	seen := map[token.Pos]bool{}
	var mentions []ast.Node
	for _, f := range pkgFiles {
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && isLocationLiteral(lit) {
				mentions = append(mentions, lit)
			}
			return true
		})
	}

	var out []locationProducer
	for _, f := range pkgFiles {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			strParams := stringParams(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isHeaderMutation(call) || len(call.Args) == 0 || !isLocationLiteral(call.Args[0]) {
					return true
				}
				seen[call.Args[0].Pos()] = true
				if sel := call.Fun.(*ast.SelectorExpr); sel.Sel.Name == "Del" || sel.Sel.Name == "Get" {
					return true // reads and deletes write no address
				}
				if len(call.Args) < 2 {
					t.Fatalf("%s: Location %s with no value argument; this guard does not understand the shape",
						pos(call), call.Fun.(*ast.SelectorExpr).Sel.Name)
				}
				atoms, ok := resolveAtoms(call.Args[1], consts, strParams)
				if !ok {
					t.Fatalf("%s: this guard cannot resolve the Location expression. That is a FAILURE, not a pass: "+
						"an address it cannot read is an address it cannot check. Teach resolveAtoms the shape.", pos(call))
				}
				out = append(out, expandProducer(t, pkgFiles, fn, call, atoms, consts, pos)...)
				return true
			})
		}
	}

	for _, m := range mentions {
		if !seen[m.Pos()] {
			p := fset.Position(m.Pos())
			t.Fatalf("%s:%d: a %q string this guard did not account for. Fail closed: teach the guard the shape "+
				"rather than letting an unread Location write ship.", filepath.Base(p.Filename), p.Line, "Location")
		}
	}
	return out
}

// expandProducer turns one Location write into the addresses it can actually produce. A write whose
// prefix is a PARAMETER (the per-handler `write` renderers all take a locationPrefix) produces one
// address per call site, and the call sites are what carry the literals.
func expandProducer(t *testing.T, pkgFiles map[string]*ast.File, fn *ast.FuncDecl,
	call *ast.CallExpr, atoms []atom, consts map[string]string, pos func(ast.Node) string) []locationProducer {
	t.Helper()

	var param string
	for _, a := range atoms {
		if a.param != "" {
			if param != "" && param != a.param {
				t.Fatalf("%s: Location built from two different parameters; extend the guard", pos(call))
			}
			param = a.param
		}
	}
	if param == "" {
		return []locationProducer{{where: pos(call), addr: render(atoms)}}
	}

	idx, ok := paramIndex(fn, param)
	if !ok {
		t.Fatalf("%s: %q is not a parameter of %s", pos(call), param, fn.Name.Name)
	}
	// The renderers write the header only when the prefix is non-empty. Model that CONDITION rather
	// than assuming it: if the write is not guarded, a call site passing "" writes a prefix-less
	// Location and the guard must see it.
	guarded := writeIsGuardedOnEmpty(fn, call, param)

	// Call sites are matched by receiver TYPE, not receiver name. Six handlers in this package each
	// declare a `write` method and every one of them names its receiver `h`, so matching on the name
	// alone would attribute one handler's prefixes to another. A call `h.write(...)` belongs to this
	// method exactly when it sits inside a method of the SAME receiver type whose receiver is `h`.
	recvType := receiverType(fn)
	var out []locationProducer
	sites := 0 // counted apart from `out`: a helper every caller passes "" to writes no header at all,
	// which is legitimate, whereas a helper whose callers the guard cannot FIND is a hole in the sweep.
	for _, f := range pkgFiles {
		for _, decl := range f.Decls {
			caller, ok := decl.(*ast.FuncDecl)
			if !ok || caller.Body == nil {
				continue
			}
			callerRecv := receiverName(caller)
			ast.Inspect(caller.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != fn.Name.Name {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				// A selector on something other than the enclosing receiver is a call this guard cannot
				// attribute to a type without full type-checking. Fail closed rather than guess.
				if id.Name != callerRecv || receiverType(caller) == "" {
					if id.Name == receiverName(fn) {
						t.Fatalf("%s: %s.%s is called on something other than an enclosing receiver; this guard "+
							"resolves receiver-qualified calls only. Widen it.", pos(c), id.Name, fn.Name.Name)
					}
					return true
				}
				if receiverType(caller) != recvType {
					return true // a different handler's identically-named helper
				}
				if idx >= len(c.Args) {
					return true
				}
				sites++
				siteAtoms, ok := resolveAtoms(c.Args[idx], consts, stringParams(caller))
				if !ok {
					t.Fatalf("%s: this guard cannot resolve the %s argument passed here", pos(c), param)
				}
				for _, a := range siteAtoms {
					if a.param != "" {
						t.Fatalf("%s: the Location prefix is forwarded from another parameter; this guard resolves "+
							"one level of indirection. Widen it.", pos(c))
					}
				}
				prefix := render(siteAtoms)
				if prefix == "" {
					if !guarded {
						t.Fatalf("%s: an empty Location prefix reaches an UNGUARDED write at %s, so a header with no "+
							"address is written here", pos(c), pos(call))
					}
					return true
				}
				var full []atom
				for _, a := range atoms {
					if a.param != "" {
						full = append(full, atom{lit: prefix})
						continue
					}
					full = append(full, a)
				}
				out = append(out, locationProducer{where: pos(call), via: pos(c), addr: render(full)})
				return true
			})
		}
	}
	if sites == 0 {
		t.Fatalf("%s: %s.%s writes a Location but this guard found NO call site supplying its prefix; that would "+
			"silently drop every address it renders from the sweep", pos(call), recvType, fn.Name.Name)
	}
	return out
}

// atom is one piece of a concatenated Location expression.
type atom struct {
	lit     string // literal text
	param   string // an enclosing-function string parameter, filled in from call sites
	runtime bool   // a value known only at run time (a minted id)
}

func render(atoms []atom) string {
	var b strings.Builder
	for _, a := range atoms {
		switch {
		case a.runtime:
			b.WriteString(probeSegment)
		default:
			b.WriteString(a.lit)
		}
	}
	return b.String()
}

// resolveAtoms flattens a string-concatenation expression. ok=false means "not understood", which the
// caller turns into a failure — never into a skip.
func resolveAtoms(e ast.Expr, consts map[string]string, strParams map[string]bool) ([]atom, bool) {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return resolveAtoms(v.X, consts, strParams)
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return nil, false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return nil, false
		}
		return []atom{{lit: s}}, true
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return nil, false
		}
		l, ok := resolveAtoms(v.X, consts, strParams)
		if !ok {
			return nil, false
		}
		r, ok := resolveAtoms(v.Y, consts, strParams)
		if !ok {
			return nil, false
		}
		return append(l, r...), true
	case *ast.Ident:
		if strParams[v.Name] {
			return []atom{{param: v.Name}}, true
		}
		if lit, ok := consts[v.Name]; ok {
			return []atom{{lit: lit}}, true
		}
		// A local holding a minted id. Anything else that lands here renders as a bare probe segment
		// and will not resolve, which is the fail-closed direction.
		return []atom{{runtime: true}}, true
	case *ast.CallExpr, *ast.SelectorExpr, *ast.IndexExpr:
		return []atom{{runtime: true}}, true
	}
	return nil, false
}

// ---- small AST helpers -----------------------------------------------------------------------------

// isHeaderMutation matches x.Header().Set/Add/Del/Get(...) — every way this tree touches a header by name.
func isHeaderMutation(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Set", "Add", "Del", "Get":
	default:
		return false
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	isel, ok := inner.Fun.(*ast.SelectorExpr)
	return ok && isel.Sel.Name == "Header"
}

// isLocationLiteral matches the header name case-insensitively, because net/http canonicalises it and
// a lower-case spelling would set the very same header.
func isLocationLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && strings.EqualFold(s, "Location")
}

func packageStringConsts(files map[string]*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							out[name.Name] = s
						}
					}
				}
			}
		}
	}
	return out
}

func stringParams(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if fn.Type.Params == nil {
		return out
	}
	for _, field := range fn.Type.Params.List {
		if id, ok := field.Type.(*ast.Ident); !ok || id.Name != "string" {
			continue
		}
		for _, n := range field.Names {
			out[n.Name] = true
		}
	}
	return out
}

func paramIndex(fn *ast.FuncDecl, name string) (int, bool) {
	i := 0
	for _, field := range fn.Type.Params.List {
		for _, n := range field.Names {
			if n.Name == name {
				return i, true
			}
			i++
		}
		if len(field.Names) == 0 {
			i++
		}
	}
	return 0, false
}

func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 || len(fn.Recv.List[0].Names) == 0 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

// receiverType is the bare type name a method hangs off, pointer stripped; "" for a plain function.
func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	e := fn.Recv.List[0].Type
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// writeIsGuardedOnEmpty reports whether the Location write sits inside `if <param> != ""`.
func writeIsGuardedOnEmpty(fn *ast.FuncDecl, call *ast.CallExpr, param string) bool {
	guarded := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		bin, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok || bin.Op != token.NEQ {
			return true
		}
		id, ok := bin.X.(*ast.Ident)
		if !ok || id.Name != param {
			return true
		}
		lit, ok := bin.Y.(*ast.BasicLit)
		if !ok || lit.Value != `""` {
			return true
		}
		if ifs.Body.Pos() <= call.Pos() && call.End() <= ifs.Body.End() {
			guarded = true
		}
		return true
	})
	return guarded
}
