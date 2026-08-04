//go:build component

package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/storage"
)

// TestEveryFleetRouteRefusesATenantKey is Faz A.1 Task 3 (2026-08-03): the machine fleet — runners,
// runner-pools, runner-pool-keys — is NOT a customer surface. In a hosted deployment no tenant key may
// even learn which machines exist; in a self-hosted one the operator's own key carries `system` (Task 1's
// middleware.ScopeSystem / Scope.HasSystem()). router.go's systemOnly wrapper is what enforces that.
//
// THE ROUTE LIST IS NOT HAND-TYPED. fleetRoutesFromSource reads apps/control-plane/api/router.go OFF DISK
// and extracts every "VERB /v1/runners…|/v1/runner-pools…|/v1/runner-pool-keys…" registration in it — the
// same grep the task brief opens with, run as part of the test itself rather than trusted as a comment. A
// route added to the fleet block next week is swept in with no change to this file; a route renamed out of
// the three prefixes drops out of the count along with it. len(routes)==0 fails loudly rather than quietly
// driving zero requests — a filter that stops matching must not read as a passing gate.
//
// BOTH DIRECTIONS ARE ASSERTED, not only the refusal. A systemOnly with a bug that rejected EVERY caller —
// not only tenant keys — would still make a test that checks ONLY the tenant-key-gets-403 direction pass.
// The system-scoped key here also carries `provision` and `approve`, the two UNRELATED capabilities several
// of these same routes separately require (api/runners.go's authorizeAdmin / approveScope) — without them a
// system-only key would legitimately 403 on those routes for a reason that has nothing to do with this
// task's gate, and the assertion would misattribute the failure. So the system key must clear every route's
// TOTAL authorization, and the only thing this test attributes to systemOnly is the tenant key's rejection.
func TestEveryFleetRouteRefusesATenantKey(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The REAL fleet registry and the REAL desired-config store, both over this Postgres — not fakes.
	// systemOnly runs before either is ever consulted for a rejected caller, but a system-scoped caller
	// must reach a genuine handler rather than a stub that could paper over a wiring mistake.
	runnerRegistry := fleet.NewStore(repo.Spine().Pool(), middleware.NewID, nil)
	runnerAPI := fleet.NewRegistryAPI(runnerRegistry, nil)

	// The "system" key's project must be a REAL row: every fleet handler now resolves its organization
	// fresh from the project (A.2 Task 3, storage.OrganizationForProject) before it does anything else, so
	// a synthetic project id with no backing row would turn every route's success case into a 500 the
	// assertion below (checks only for 401/403) would never catch — proving nothing about systemOnly and
	// everything about a missing fixture row instead.
	sys := storage.WithSystemScope(ctx)
	if _, err := repo.Spine().Pool().Exec(sys, `INSERT INTO projects (id) VALUES ($1)`,
		"prj_fleet_system"); err != nil {
		t.Fatalf("seed prj_fleet_system: %v", err)
	}

	verifier := keyedVerifier{
		"tenant": {Project: "prj_fleet_tenant", Principal: "prin_fleet_tenant"},
		// provision + approve so the ONLY thing standing between this key and every fleet route is
		// whichever capability systemOnly checks — see the doc comment above.
		"system": {Project: "prj_fleet_system", Principal: "prin_fleet_system",
			Scopes: []string{middleware.ScopeSystem, "provision", "approve"}},
	}
	router := api.NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithRunners(runnerAPI), api.WithDesiredConfig(repo))
	ts := httptest.NewServer(router)
	defer ts.Close()

	routes := fleetRoutesFromSource(t)
	if len(routes) == 0 {
		t.Fatal("no fleet route matched router.go's own source — the pattern stopped matching the tree's shape, " +
			"which would make this guard VACUOUS (zero routes driven, test still green)")
	}
	t.Logf("kapsanan filo rotası: %d", len(routes))

	for _, rt := range routes {
		path := rt.fillPath()

		res := fleetKeyedDo(t, ts.URL, rt.method, path, "tenant")
		if res != http.StatusForbidden {
			t.Errorf("%s %s bir tenant anahtarını kabul etti: %d (beklenen 403)", rt.method, rt.path, res)
		}

		res = fleetKeyedDo(t, ts.URL, rt.method, path, "system")
		if res == http.StatusUnauthorized || res == http.StatusForbidden {
			t.Errorf("%s %s system kapsamı taşıyan bir anahtarı da reddetti: %d — kapı operatörü de dışlıyor",
				rt.method, rt.path, res)
		}
	}
}

// ---- the route inventory: read from router.go's own source, never pasted ---------------------------

// fleetRoute is one registration this test drives.
type fleetRoute struct {
	method string
	path   string // as written in router.go, wildcards and all ("{runner_id}")
}

// fillPath replaces every {wildcard} path segment with a legal single-segment probe value, so the request
// this test sends is one Go's ServeMux actually dispatches rather than one that never matches a pattern.
func (rt fleetRoute) fillPath() string {
	segs := strings.Split(rt.path, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "probe"
		}
	}
	return strings.Join(segs, "/")
}

// fleetRoutePattern is Step 1 of the task brief's own grep, kept as a regexp instead of a pasted list:
//
//	grep -roE '"(GET|POST|PATCH|DELETE) /v1/(runners|runner-pools|runner-pool-keys)[^"]*"' router.go
//
// Run here as CODE against the file on disk, so the inventory is regenerated every time this test runs
// rather than trusted from a comment written on some earlier day.
var fleetRoutePattern = regexp.MustCompile(`"(GET|POST|PATCH|DELETE) (/v1/(?:runners|runner-pools|runner-pool-keys)[^"]*)"`)

// fleetRoutesFromSource parses apps/control-plane/api/router.go OFF DISK and returns the deduplicated,
// sorted set of routes registered under the fleet's three path prefixes.
func fleetRoutesFromSource(t *testing.T) []fleetRoute {
	t.Helper()
	root := repoRootFromStoreTest(t)
	src, err := os.ReadFile(filepath.Join(root, "apps/control-plane/api/router.go"))
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	matches := fleetRoutePattern.FindAllStringSubmatch(string(src), -1)
	seen := map[string]bool{}
	var out []fleetRoute
	for _, m := range matches {
		key := m[1] + " " + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, fleetRoute{method: m[1], path: m[2]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].method < out[j].method
	})
	return out
}

// repoRootFromStoreTest walks up to the module root so the test can read router.go's source directly. It
// is a package-local copy of the same helper api/capabilities_tier_test.go carries (repoRootFromTest) —
// that one is unexported to package api and this package cannot see it.
func repoRootFromStoreTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// fleetKeyedDo sends one request under the named keyedVerifier token and returns the status code. Every
// fleet route this test drives is gated BEFORE any body is read, so an empty JSON object is a safe body for
// every method including the writes.
func fleetKeyedDo(t *testing.T, base, method, path, token string) int {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodGet {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
