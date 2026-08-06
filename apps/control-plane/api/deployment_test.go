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
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	"PALAI_WORKER_ENROLL_TOKEN",      // fresh per boot, re-presentable within that boot
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
//
// IT RESOLVES ONE CONSTANT HOP, and that is a correctness fix rather than a loosening. A reader that spells
// the variable as a named constant — `os.Getenv(fakeScriptFileEnv)`, which main.go does so its refusal text
// and its read cannot drift apart — reads the setting exactly as much as a literal does, and a substring
// scan cannot see it. Rejecting it would have pushed the next person toward the WORSE code (a second
// spelling of the name at the call site) to satisfy the guard, which is the guard bending the tree. The hop
// is narrow on purpose: the constant must be declared in the SAME file and its value must equal the setting
// name exactly, so a function that names neither still fails.
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
		// The names the file binds to this exact setting string, so a read through one of them counts.
		aliases := constantsBoundTo(file, entry.Name)
		found := false
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != entry.ReaderFunc {
				continue
			}
			found = true
			body := string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
			mentions := strings.Contains(body, entry.Name)
			for _, alias := range aliases {
				mentions = mentions || strings.Contains(body, alias)
			}
			if !mentions {
				t.Errorf("%s cites %s.%s, but that function's source never mentions %s (nor any of the file's constants bound to it: %v) — the citation points at code that does not read the setting",
					entry.Name, entry.ReaderFile, entry.ReaderFunc, entry.Name, aliases)
			}
		}
		if !found {
			t.Errorf("%s cites %s.%s, and that file declares no such function", entry.Name, entry.ReaderFile, entry.ReaderFunc)
		}
	}
}

// constantsBoundTo returns the identifiers a file declares as `const NAME = "<setting>"`. It resolves the ONE
// indirection a reader may legitimately put between itself and the setting name; anything further (a variable
// reassigned at runtime, a name built by concatenation, a constant from another package) is deliberately not
// followed, because past that point the guard would be guessing what the code reads rather than reading it.
func constantsBoundTo(file *ast.File, setting string) []string {
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if unquoted, err := strconv.Unquote(lit.Value); err == nil && unquoted == setting {
					names = append(names, name.Name)
				}
			}
		}
	}
	return names
}

// TestDeploymentRefusesAKeyWithoutTheProvisionCapability. A machine's configuration is an OPERATOR read,
// not a tenant one: it names the object store, the sandbox image, the GitHub App and every path the process
// holds open. It is gated on the same `provision` capability the tenancy surface uses, so a narrow
// project-scoped key sent to a run cannot enumerate the deployment it is running on.
func TestDeploymentRefusesAKeyWithoutTheProvisionCapability(t *testing.T) {
	// A key with a NON-EMPTY, narrow scope set. The bootstrap key the console signs in with carries no
	// scopes at all, which middleware.Scope.HasScope reads as unrestricted — so an empty set here would
	// pass the gate and prove nothing.
	narrow := scopedVerifier{scope: middleware.Scope{Project: "prj_1", Principal: "prin_1", Scopes: []string{"responses"}}}
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

// The console prints the majority remedy ONCE, above the table, instead of once per row — twenty-nine
// identical sentences down a column is the shape of the column drowning the four rows that say something
// else, and one of those four says a stored secret rotates live.
//
// To do that it carries the command as a LITERAL (apps/web-console/app/deployment/page.tsx's
// SHARED_REMEDY_COMMAND), which is a copy of a string this file owns. A copy is a drift waiting to happen
// and the drift is INVISIBLE: reword the catalogue and the console keeps printing the old command above a
// table that no longer says it, with nothing red anywhere. So the copy is not trusted — it is read out of
// the .tsx and checked against what the catalogue actually serves.
//
// It asserts against the MAJORITY sentence rather than any sentence, because that is what the console
// suppresses. A remedy that appears on one row is printed on that row and needs no notice.
func TestTheDeploymentPageHardcodesNoRemedySentence(t *testing.T) {
	const page = "../../web-console/app/deployment/page.tsx"
	source, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}

	// EVERY DISTINCT REMEDY, and the sentence has to be long enough for the containment test to mean
	// something — a two-word remedy would match incidental prose and this guard would fail for the one
	// reason it is not about.
	seen := map[string]bool{}
	for _, setting := range deploymentCatalogue {
		if setting.ChangeWith == "" || seen[setting.ChangeWith] {
			continue
		}
		seen[setting.ChangeWith] = true
		if len(setting.ChangeWith) < 40 {
			t.Fatalf("%s's remedy is only %d characters (%q); this guard tests for it as a SUBSTRING of a page, "+
				"and a short sentence would match incidentally", setting.Name, len(setting.ChangeWith), setting.ChangeWith)
		}
		if strings.Contains(string(source), setting.ChangeWith) {
			t.Fatalf("%s writes a remedy sentence into the page as a literal:\n  %q\nThe screen renders each "+
				"remedy ONCE, derived from the rows it loaded, precisely so there is no second copy able to drift "+
				"from the catalogue. A sentence typed here is that second copy, and the drift is invisible until an "+
				"operator follows a command this deployment no longer names.", page, setting.ChangeWith)
		}
	}
	// NON-VACUITY: a loop over an empty catalogue asserts nothing, and a renamed field would produce exactly
	// that silence.
	if len(seen) < 2 {
		t.Fatalf("the catalogue yielded %d distinct remedy sentence(s); with fewer than two this guard cannot "+
			"distinguish a page that derives them from one that hardcodes them", len(seen))
	}
}

// TestEveryCatalogueEntryExplainsWhatItDoes guards the field the console renders as "What it does".
//
// It was worth adding the moment that column existed, and not before: `effect` had been in the
// catalogue and on the wire since it was written, and NOTHING displayed it — so an entry added with an
// empty one broke nothing an operator could see. The column changes that. An empty cell on a
// configuration screen is worse than an absent column, because it reads as "this setting does nothing"
// rather than "nobody wrote it down".
//
// The check is emptiness only, deliberately. Whether a sentence is a GOOD explanation is not a property
// a test can hold, and the neighbouring citation guard already proves the sentence is about a variable
// something really reads.
func TestEveryCatalogueEntryExplainsWhatItDoes(t *testing.T) {
	for _, entry := range deploymentCatalogue {
		if strings.TrimSpace(entry.Effect) == "" {
			t.Errorf("%s has no effect sentence: the console's \"What it does\" column would render an empty "+
				"cell, which an operator reads as a setting that does nothing", entry.Name)
		}
	}
}

// compositionRootReads is every PALAI_ name the control-plane binary's composition root reads. It scans
// the SOURCE, because the question is what the process consults — not what somebody remembered to write
// down, which is the thing being checked.
var compositionRootRead = regexp.MustCompile(`"(PALAI_[A-Z0-9_]+)"`)

func compositionRootReads(t *testing.T) []string {
	t.Helper()
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "apps/control-plane/cmd/palai-control-plane/main.go"))
	if err != nil {
		t.Fatalf("read the composition root: %v", err)
	}
	seen := map[string]bool{}
	var names []string
	for _, m := range compositionRootRead.FindAllStringSubmatch(string(body), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
	}
	sort.Strings(names)
	return names
}

// TestEverySettingTheCompositionRootReadsIsAccountedFor closes the direction nothing in this file looked.
//
// ‼️ THE GUARDS HERE WALK COMPOSE -> CATALOGUE, AND CATALOGUE -> COMPOSE FOR THE WRITABLE ENTRIES. A
// variable the BINARY reads that no compose file names is invisible to both, and one sat in that blind
// spot until 2026-08-06: PALAI_WORKSPACE_IDLE_TTL decides when EVERY session's machine is archived and
// handed back, appeared in zero deploy files, zero docs and no catalogue, and nothing was red — because
// it has a default, so the sweep ran and no operator could see or change the number it ran with.
//
// Four ways to be accounted for, and each says something different: catalogued (an operator can read it),
// declared unreported (its value must not reach the surface), a secret-file PREFIX (there is no fixed
// name to hold), or written into uncataloguedSettings with a reason. The fourth is a debt ledger, and
// this test is what makes it a CEILING: a name added to main.go and to none of the four fails here.
func TestEverySettingTheCompositionRootReadsIsAccountedFor(t *testing.T) {
	catalogued := map[string]bool{}
	for _, entry := range deploymentCatalogue {
		catalogued[entry.Name] = true
	}
	// A positive control: the scan must find a name this file certainly catalogues, or it is reading the
	// wrong file (or the wrong pattern) and its silence means nothing.
	reads := compositionRootReads(t)
	var sawCatalogued bool
	for _, name := range reads {
		if catalogued[name] {
			sawCatalogued = true
			break
		}
	}
	if !sawCatalogued {
		t.Fatal("the scan of the composition root found no catalogued setting at all — it is reading the " +
			"wrong file or has stopped matching, and every assertion below would pass on an empty set")
	}

	credential := map[string]bool{}
	for _, name := range credentialBearingSettings {
		credential[name] = true
	}
	for _, name := range reads {
		switch {
		case catalogued[name]:
		case unreportedSettings[name] != "":
		case credential[name]:
		case uncataloguedSettings[name] != "":
		default:
			if isSecretFilePrefix(name) {
				continue
			}
			t.Errorf("the composition root reads %s and nothing accounts for it. An operator cannot see it, "+
				"cannot change it from any deployment file this repository ships, and no guard would notice "+
				"if its default were wrong. Catalogue it, declare it unreported, or add it to "+
				"uncataloguedSettings WITH the reason it is not yet a row", name)
		}
	}

	// The ledger may not name something that is already accounted for elsewhere: two sources of truth about
	// one setting is how a stale exemption survives the change that made it a real entry.
	for name := range uncataloguedSettings {
		if catalogued[name] || unreportedSettings[name] != "" {
			t.Errorf("uncataloguedSettings names %s, which is already catalogued or declared unreported — "+
				"delete the ledger entry, it is the one that will go stale", name)
		}
	}
}

func isSecretFilePrefix(name string) bool {
	for _, prefix := range secretFilePrefixes {
		if name == prefix {
			return true
		}
	}
	return false
}

// callSiteDefault matches the two readers that carry their fallback AT the call site, which is the only
// place a default is expressed as a value rather than as prose.
var callSiteDefault = regexp.MustCompile(`env(?:IntDefault|DurationOr)\("(PALAI_[A-Z0-9_]+)",\s*([^)]+)\)`)

// namedConstant resolves `pkg.Name` to the expression its declaration assigns, so a default written as a
// constant is still comparable. It returns "" when the name is not a simple package-qualified constant.
func namedConstant(t *testing.T, expr string) string {
	t.Helper()
	if !strings.Contains(expr, ".") || strings.ContainsAny(expr, "*+ ") {
		return ""
	}
	name := expr[strings.LastIndex(expr, ".")+1:]
	// ‼️ POSIX ERE, NOT PERL: git grep -E has no `\s`, and the first draft's `^\s*` matched nothing at all —
	// which returned "" and reported the catalogue wrong while the PROPERTY held.
	cmd := exec.Command("git", "grep", "-h", "-E", "^[[:space:]]*(const |var )?"+name+"[[:space:]]*=")
	// ‼️ FROM THE REPO ROOT. `git grep` searches the CURRENT DIRECTORY's subtree, and this test runs in
	// apps/control-plane/api while the constants it resolves live under internal/. The third draft found
	// nothing for that reason alone and reported the catalogue wrong — a sweep that looks in the wrong place
	// returns exactly what a sweep that found nothing returns.
	cmd.Dir = repoRootFromTest(t)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		// ‼️ AND COMMENTS ARE SKIPPED, because the second draft resolved the constant from THIS FILE's own
		// prose about it. A resolver that reads the sentence explaining a value instead of the value is the
		// same defect as a guard that checks a claim instead of the world.
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
			continue
		}
		if _, rhs, ok := strings.Cut(line, "="); ok {
			return strings.TrimSpace(rhs)
		}
	}
	return ""
}

// renderDefault turns a Go duration/int expression into the spelling a human writes: `2*time.Minute` ->
// "2m", `time.Hour` -> "1h", `256` -> "256".
func renderDefault(expr string) string {
	// Spaces removed first: a call site writes `2*time.Minute` and a declaration writes `5 * time.Minute`,
	// and the same value must render the same way from either.
	expr = strings.ReplaceAll(strings.TrimSpace(expr), " ", "")
	units := []struct {
		suffix string
		short  string
	}{{"time.Hour", "h"}, {"time.Minute", "m"}, {"time.Second", "s"}}
	for _, u := range units {
		if expr == u.suffix {
			return "1" + u.short
		}
		if n, ok := strings.CutSuffix(expr, "*"+u.suffix); ok {
			return strings.TrimSpace(n) + u.short
		}
	}
	return expr
}

// TestACataloguedDefaultIsTheOneTheCodeApplies binds the catalogue's `Default` PROSE to the value the
// composition root actually falls back to.
//
// ‼️ NOTHING MEASURED THAT FIELD, AND IT IS THE ONE AN OPERATOR RELIES ON. Measured 2026-08-06 by writing
// a deliberate lie — "45m" for a five-minute replay window — and running the whole package: green. Every
// other guard here checks a name, a reader, a plane or a refusal; the sentence telling somebody what their
// deployment does when they set NOTHING was free prose.
//
// ‼️ AND ITS COVERAGE IS A CEILING, STATED RATHER THAN IMPLIED. Only a default expressed at the call site
// (`envIntDefault`, `envDurationOr`) is comparable — 19 reads today. A default that lives inside the
// consumer (`TriggerStore.tolerance()` returns 5m for an unset value) is NOT checked by this, and a
// catalogue entry for one of those is still prose somebody has to verify by hand. The count below is what
// keeps that honest: if this stops covering anything, it fails instead of reporting clean.
func TestACataloguedDefaultIsTheOneTheCodeApplies(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "apps/control-plane/cmd/palai-control-plane/main.go"))
	if err != nil {
		t.Fatalf("read the composition root: %v", err)
	}
	applied := map[string]string{}
	for _, m := range callSiteDefault.FindAllStringSubmatch(string(body), -1) {
		applied[m[1]] = strings.TrimSpace(m[2])
	}
	if len(applied) == 0 {
		t.Fatal("no call-site defaults were parsed — the pattern has stopped matching and every comparison " +
			"below would be skipped, which reads exactly like a catalogue whose defaults all agree")
	}

	checked := 0
	for _, entry := range deploymentCatalogue {
		expr, ok := applied[entry.Name]
		if !ok {
			continue
		}
		if resolved := namedConstant(t, expr); resolved != "" {
			expr = resolved
		}
		want := renderDefault(expr)
		checked++
		// ‼️ THE LEADING TOKEN, NOT A SUBSTRING. `strings.Contains("45m", "5m")` is TRUE, so the first draft
		// of this guard passed the exact lie it was written to catch — measured, not reasoned about. This
		// tree already records defeated membership comparisons as a class; a defeated one in a GUARD says
		// nothing at all, forever. The field's convention is "<value> — prose", so the value is field one.
		fields := strings.Fields(entry.Default)
		if len(fields) == 0 || fields[0] != want {
			t.Errorf("%s: the catalogue says its default is %q, and the composition root falls back to %s. "+
				"An operator reads that field to know what their deployment does when they set nothing",
				entry.Name, entry.Default, want)
		}
	}
	if checked == 0 {
		t.Fatalf("this guard compared NOTHING: %d call-site defaults were parsed and none of them names a "+
			"catalogued setting. It is not protecting the field it was written for", len(applied))
	}
}
