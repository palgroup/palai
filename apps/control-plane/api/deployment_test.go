package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// THE MACHINE'S OWN CONFIGURATION, AND WHY IT IS A ROUTE AT ALL.
//
// Measured on main at cf0efd63 (2026-08-01):
//
//	grep -oE 'PALAI_[A-Z_]+' deploy/compose/compose.yaml | sort -u   -> 24 settings
//	of those, readable from any /v1 route                            -> 0
//
// The second number is what this file exists to change, and the cost of it being zero was paid the night
// this task was written: a stack came up on `make local-up`, which defaults PALAI_DISPATCH_WORKERS to 0
// (deploy/compose/compose.yaml:82), the console accepted five runs, and every one of them sat at
// run.queued.v1 forever. Nothing on any screen said the deployment had no dispatcher. The value was
// eventually found with `docker inspect`, which an operator will not run.
//
// So the tests below are not "does the handler return 200". They are the three properties a configuration
// read surface has to have before it is worth serving:
//
//  1. IT REPORTS WHAT THE BINARY RUNS, not what a second copy of the parsing rules thinks it runs. The
//     dispatch posture is read through the SAME function main.startDispatch gates on.
//  2. IT NEVER RETURNS A CREDENTIAL. The catalogue is an ALLOW-LIST, so the failure mode of forgetting a
//     new secret-bearing variable is that it is invisible, not that it is published.
//  3. IT CANNOT SILENTLY OMIT A SETTING. compose.yaml is walked; the catalogue is a list; the two are
//     diffed in both directions. A walk finds a setting nobody catalogued, and only the list can find a
//     catalogue entry naming a setting that no longer exists.

// deploymentBodyOf drives GET /v1/deployment through the shipped router and decodes the body.
func deploymentBodyOf(t *testing.T, router http.Handler) (deploymentBody, string) {
	t.Helper()
	ts := httptest.NewServer(router)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/deployment", nil)
	req.Header.Set("Authorization", "Bearer any")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/deployment: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/deployment = %d, want 200: %s", resp.StatusCode, raw)
	}
	var body deploymentBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode deployment body: %v (%s)", err, raw)
	}
	return body, string(raw)
}

func settingNamed(body deploymentBody, name string) (deploymentSetting, bool) {
	for _, s := range body.Settings {
		if s.Name == name {
			return s, true
		}
	}
	return deploymentSetting{}, false
}

func warningCoded(body deploymentBody, code string) (deploymentWarning, bool) {
	for _, w := range body.Warnings {
		if w.Code == code {
			return w, true
		}
	}
	return deploymentWarning{}, false
}

// TestDeploymentReportsTheDispatchPostureTheBinaryRuns is the leg that pays for the whole surface.
//
// A row saying `PALAI_DISPATCH_WORKERS: 0` is not enough on its own and this test says so in both
// directions: at zero the body must carry a WARNING that names the consequence (submitted work is admitted
// and never executed), and above zero that warning must be GONE. A banner that is always on is wallpaper.
//
// The posture is read through DispatchWorkers, which main.dispatchWorkerCount delegates to, so the default
// this surface reports and the default startDispatch gates on cannot drift apart — the failure mode E29's
// own CLAUDE.md calls "a second copy of the parsing rules".
func TestDeploymentReportsTheDispatchPostureTheBinaryRuns(t *testing.T) {
	t.Setenv("PALAI_DISPATCH_WORKERS", "0")
	body, _ := deploymentBodyOf(t, bareRouter())

	row, ok := settingNamed(body, "PALAI_DISPATCH_WORKERS")
	if !ok {
		t.Fatal("GET /v1/deployment reports no PALAI_DISPATCH_WORKERS row — the setting whose zero cost this task its evening is the one the surface exists for")
	}
	if row.Value != "0" {
		t.Errorf("PALAI_DISPATCH_WORKERS value = %q, want %q", row.Value, "0")
	}
	if row.Mutability != mutabilityBringUp {
		t.Errorf("PALAI_DISPATCH_WORKERS mutability = %q, want %q: startDispatch's early return runs ONCE at boot, so nothing this process can be told changes it", row.Mutability, mutabilityBringUp)
	}
	if row.ChangeWith == "" {
		t.Error("PALAI_DISPATCH_WORKERS carries no change_with — a screen that says a value cannot be changed live and does not say what DOES change it has told the operator to go and find out")
	}

	warn, ok := warningCoded(body, warnDispatchOff)
	if !ok {
		t.Fatalf("PALAI_DISPATCH_WORKERS=0 raised no %q warning; warnings = %+v. A deployment that accepts runs and executes none must say so, on the surface, without an operator reaching for docker inspect", warnDispatchOff, body.Warnings)
	}
	if warn.Severity != severityBlocking {
		t.Errorf("%s severity = %q, want %q", warnDispatchOff, warn.Severity, severityBlocking)
	}
	if warn.Remedy == "" {
		t.Errorf("%s carries no remedy", warnDispatchOff)
	}

	t.Setenv("PALAI_DISPATCH_WORKERS", "2")
	body, _ = deploymentBodyOf(t, bareRouter())
	if _, ok := warningCoded(body, warnDispatchOff); ok {
		t.Errorf("PALAI_DISPATCH_WORKERS=2 still raises %q — a warning that never clears is decoration", warnDispatchOff)
	}
	if row, _ := settingNamed(body, "PALAI_DISPATCH_WORKERS"); row.Value != "2" {
		t.Errorf("PALAI_DISPATCH_WORKERS value = %q, want %q", row.Value, "2")
	}
}

// TestDeploymentWarnsWhenEveryRunWouldBeFabricated is the second of the two settings that silently change
// what the product DOES. `fake` executes runs to completion and produces a transcript that renders exactly
// like a real one — the dispatch-off failure is at least visible as a run that never moves; this one is
// invisible by construction.
//
// It is ADVISORY, not blocking, and that distinction is the honest one: `fake` is the shipped default of
// every deterministic tier in this tree and a legitimate posture to run in. What is not legitimate is a
// console that shows the output without saying where it came from.
func TestDeploymentWarnsWhenEveryRunWouldBeFabricated(t *testing.T) {
	t.Setenv("PALAI_MODEL_PROVIDER", "fake")
	body, _ := deploymentBodyOf(t, bareRouter())
	warn, ok := warningCoded(body, warnModelFake)
	if !ok {
		t.Fatalf("PALAI_MODEL_PROVIDER=fake raised no %q warning; warnings = %+v", warnModelFake, body.Warnings)
	}
	if warn.Severity != severityAdvisory {
		t.Errorf("%s severity = %q, want %q: the fake adapter is the shipped default of every deterministic tier, so a blocking word here would be wrong", warn.Code, warn.Severity, severityAdvisory)
	}

	// THE ROW IS `bring_up_default_only`, NOT `bring_up`, and the difference is the whole of requirement 2.
	// A project that publishes a model route dispatches through THAT, resolved per attempt
	// (internal/execution/model_route.go effectiveRoute), so the env value decides only what a project with
	// no published route gets. Calling it plain `bring_up` would send an operator to restart a stack when a
	// POST would have done.
	row, ok := settingNamed(body, "PALAI_MODEL_PROVIDER")
	if !ok {
		t.Fatal("GET /v1/deployment reports no PALAI_MODEL_PROVIDER row")
	}
	if row.Mutability != mutabilityDefaultOnly {
		t.Errorf("PALAI_MODEL_PROVIDER mutability = %q, want %q — a published project route overrides it with no restart", row.Mutability, mutabilityDefaultOnly)
	}

	t.Setenv("PALAI_MODEL_PROVIDER", "provider-one")
	body, _ = deploymentBodyOf(t, bareRouter())
	if _, ok := warningCoded(body, warnModelFake); ok {
		t.Errorf("PALAI_MODEL_PROVIDER=provider-one still raises %q", warnModelFake)
	}
}

// TestDeploymentNeverReportsACredentialValue is the sweep, and its GREENNESS IS THE PROOF OF AN ABSENCE —
// the same shape as the console's secret-never-returns.spec.ts.
//
// Every variable the catalogue declares UNREPORTABLE is set to a distinctive sentinel, and the sentinel is
// then looked for in the whole serialized body. The sentinel is per-variable so a failure names the leak.
//
// This is why the catalogue is an allow-list. A deny-list gets the sign wrong on the day someone adds
// PALAI_SECRET_PROVIDER_THREE: with a deny-list the new variable is PUBLISHED until somebody remembers to
// deny it; with an allow-list it is invisible until somebody decides it is safe.
func TestDeploymentNeverReportsACredentialValue(t *testing.T) {
	for name := range unreportedSettings {
		t.Setenv(name, "PALAI-LEAK-SENTINEL-"+name)
	}
	// The catalogue's own path-valued entries are set to a real-looking path, so the test cannot pass by
	// the surface reporting nothing at all.
	t.Setenv("PALAI_SECRET_MASTER_KEY_FILE", "/run/secrets/master_key")

	body, raw := deploymentBodyOf(t, bareRouter())

	for name, reason := range unreportedSettings {
		if strings.Contains(raw, "PALAI-LEAK-SENTINEL-"+name) {
			t.Errorf("the value of %s reached the response body, and it is declared unreportable: %s", name, reason)
		}
		if _, ok := settingNamed(body, name); ok {
			t.Errorf("%s appears as a settings row; an unreported variable must not be a row at all (a row with an empty value still tells a reader the shape of the deployment's secrets)", name)
		}
	}

	// The path-valued entry IS reported, by path, and that is the credential rule in this tree: a handle is
	// not a secret. If this stops holding the surface has become useless rather than safe.
	row, ok := settingNamed(body, "PALAI_SECRET_MASTER_KEY_FILE")
	if !ok {
		t.Fatal("PALAI_SECRET_MASTER_KEY_FILE is not reported at all — the master key's PATH is what makes the secret store's posture visible, and a path is not a key")
	}
	if row.Kind != kindPath {
		t.Errorf("PALAI_SECRET_MASTER_KEY_FILE kind = %q, want %q", row.Kind, kindPath)
	}
	if row.Value != "/run/secrets/master_key" {
		t.Errorf("PALAI_SECRET_MASTER_KEY_FILE value = %q, want the configured path", row.Value)
	}
}

// credentialBearingEnv names variables whose VALUE is a credential, or carries one. None of them may ever
// be catalogued.
//
// THIS IS A SECOND LINE, NOT THE FIRST ONE, and the distinction matters because this tree has learned that a
// name-based membership check is guilty until proven otherwise — four of them shipped defeated in E18 alone.
// The load-bearing protection is the ALLOW-LIST: a variable nobody catalogued is invisible, whatever it is
// called. This list can therefore only ever fail to catch something; it can never be the reason something
// leaks. What it buys is that the intent is testable — "PALAI_DATABASE_URL must not appear on an operator's
// screen" becomes a failing test rather than a sentence in a review.
//
// PALAI_DATABASE_URL is here and NOT in unreportedSettings, because compose does not set it: the production
// entrypoint bridges it from /run/secrets at start, so the compose walk never sees it and only this list can
// speak about it.
var credentialBearingEnv = []string{
	"PALAI_DATABASE_URL",             // the Postgres URL, password inline
	"PALAI_S3_ACCESS_KEY",            // object-store credential
	"PALAI_S3_SECRET_KEY",            // object-store credential
	"PALAI_SECRET_PROVIDER_ONE",      // a model provider's raw API key (modelBrokerFromEnv's EnvResolver)
	"PALAI_SECRET_PROVIDER_TWO",      // ditto
	"PALAI_SECRET_OPENAI_COMPATIBLE", // ditto
	"PALAI_CONSOLE_PASSWORD_HASH",    // the console door's scrypt hash
	"PALAI_API_KEY",                  // a bearer token
	"PALAI_WORKER_ENROLL_TOKEN",      // a one-use enrolment credential
}

func TestNoCredentialBearingVariableIsCatalogued(t *testing.T) {
	catalogued := map[string]bool{}
	for _, entry := range deploymentCatalogue {
		catalogued[entry.Name] = true
	}
	for _, name := range credentialBearingEnv {
		if catalogued[name] {
			t.Errorf("%s is in deploymentCatalogue, and its VALUE is a credential. A *_FILE path is a handle and may be reported; the secret itself may not", name)
		}
	}
}

// TestEveryComposeSettingIsCataloguedOrDeclaredUnreported is the walk-vs-list guard, and it runs in BOTH
// directions because a sweep only ever looks one way (CLAUDE.md, "Ölçüm disiplini"):
//
//   - WALK -> LIST finds a setting compose.yaml ships that no catalogue entry mentions. That is the hole
//     this whole task exists to close, reopening one variable at a time.
//   - LIST -> WALK finds a catalogue entry naming a variable compose no longer sets. A walk structurally
//     cannot find that one: it is a row about a setting that does not exist.
//
// The runner service is swept too, and its variables are expected to land in `unreportedSettings` with the
// reason: PALAI_RUNNER_CONCURRENCY lives on the RUNNER container, and cmd/cli/internal/stack/upgrade.go
// says it plainly — "reading a runner-scoped var off the control-plane container always misses it". A
// control plane that reported it would be reporting its own environment and calling it the runner's.
func TestEveryComposeSettingIsCataloguedOrDeclaredUnreported(t *testing.T) {
	shipped := composeSettingNames(t)
	if len(shipped) < 20 {
		t.Fatalf("the compose sweep found %d settings; the measurement this surface was built against found 24, so the parse is wrong and every assertion below it is vacuous", len(shipped))
	}

	catalogued := map[string]bool{}
	for _, entry := range deploymentCatalogue {
		catalogued[entry.Name] = true
	}

	for _, name := range shipped {
		if catalogued[name] || unreportedSettings[name] != "" {
			continue
		}
		t.Errorf("deploy/compose/compose.yaml sets %s and the deployment surface neither reports it nor declares why it does not. "+
			"Add a deploymentCatalogue entry, or an unreportedSettings reason — an operator cannot see a setting nobody decided about", name)
	}

	inCompose := map[string]bool{}
	for _, name := range shipped {
		inCompose[name] = true
	}
	// Variables read from the process environment but not SET by compose are legitimate (an operator or a
	// native launch may export them), so the reverse direction only fails for a catalogue entry naming a
	// variable NO reader in the tree mentions — checked by the citation guard below rather than here. What
	// this half pins is the unreported list, which exists solely to explain compose's own keys.
	for name, reason := range unreportedSettings {
		if reason == "" {
			t.Errorf("unreportedSettings[%s] carries no reason — an exemption nobody has to justify is a place to hide a setting", name)
		}
		if !inCompose[name] {
			t.Errorf("unreportedSettings names %s, which deploy/compose/compose.yaml no longer sets — a stale exemption widens the rule for a variable that may come back meaning something else", name)
		}
	}
}

// TestEveryCatalogueCitationResolvesToARealReader is the anti-rot leg, and it is the CLAUDE.md rule
// ("Çalışma zamanı durumu hakkındaki her iddia bir YAZAR gösterir") made mechanical.
//
// Every catalogue entry claims a reader: a file and the FUNCTION inside it that reads the variable. The
// guard parses that file and asserts the named function's own source text mentions the variable. So a
// mutability claim is not a sentence somebody typed — it is anchored to code that can be read, and moving
// the reader without updating the citation is a failing test rather than a comment that quietly stops
// being true.
//
// The citation is a FUNCTION and not a line number on purpose: a line number reddens on every unrelated
// edit above it, which trains people to update citations without reading them.
func TestEveryCatalogueCitationResolvesToARealReader(t *testing.T) {
	root := repoRootFromTest(t)
	for _, entry := range deploymentCatalogue {
		if entry.ReaderFile == "" || entry.ReaderFunc == "" {
			t.Errorf("%s cites no reader; a mutability claim with no writer behind it is the claim 'nothing reads this'", entry.Name)
			continue
		}
		path := filepath.Join(root, entry.ReaderFile)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s cites %s, which does not exist: %v", entry.Name, entry.ReaderFile, err)
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Errorf("%s cites %s, which does not parse: %v", entry.Name, entry.ReaderFile, err)
			continue
		}
		found := false
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != entry.ReaderFunc {
				continue
			}
			found = true
			body := string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
			if !strings.Contains(body, entry.Name) {
				t.Errorf("%s cites %s.%s, but that function's source never mentions %s — the citation points at code that does not read the setting",
					entry.Name, entry.ReaderFile, entry.ReaderFunc, entry.Name)
			}
		}
		if !found {
			t.Errorf("%s cites %s.%s, and that file declares no such function", entry.Name, entry.ReaderFile, entry.ReaderFunc)
		}
	}
}

// TestDeploymentRefusesAKeyWithoutTheProvisionCapability. A machine's configuration is an OPERATOR read,
// not a tenant one: it names the object store, the sandbox image, the GitHub App and every path the process
// holds open. It is gated on the same `provision` capability the tenancy surface uses, so a narrow
// project-scoped key sent to a run cannot enumerate the deployment it is running on.
func TestDeploymentRefusesAKeyWithoutTheProvisionCapability(t *testing.T) {
	// A key with a NON-EMPTY, narrow scope set. The bootstrap key the console signs in with carries no
	// scopes at all, which middleware.Scope.HasScope reads as unrestricted — so an empty set here would
	// pass the gate and prove nothing.
	narrow := scopedVerifier{scope: middleware.Scope{Organization: "org_1", Project: "prj_1", Principal: "prin_1", Scopes: []string{"responses"}}}
	router := NewRouter(narrow, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil)
	ts := httptest.NewServer(router)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/deployment", nil)
	req.Header.Set("Authorization", "Bearer narrow")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/deployment: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a key without the provision capability got %d, want 403", resp.StatusCode)
	}
}

// composeSettingNames walks the shipped compose file's service environment blocks and returns every
// PALAI_* key it SETS. It is the WALK half of the guard above: it reports what exists, which is exactly
// what a hand-maintained list cannot.
func composeSettingNames(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "deploy/compose/compose.yaml"))
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode compose.yaml: %v", err)
	}
	seen := map[string]bool{}
	var names []string
	for _, svc := range doc.Services {
		for key := range svc.Environment {
			if !strings.HasPrefix(key, "PALAI_") || seen[key] {
				continue
			}
			seen[key] = true
			names = append(names, key)
		}
	}
	return names
}
