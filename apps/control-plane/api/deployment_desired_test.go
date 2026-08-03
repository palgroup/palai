package api

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// interpolationPattern matches compose's `${NAME}` and `${NAME:-default}` / `${NAME-default}` forms. It is
// deliberately narrow: `$NAME` without braces is legal compose too, and no file in this tree uses it — a
// pattern that matched it would also match every `$` in a default value's prose.
var interpolationPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)[^}]*\}`)

// THE DESIRED CONFIGURATION — the WRITE half, and the four properties it has to have before a form is
// worth putting in front of an operator.
//
// The read half (deployment_test.go) exists because thirty-five settings decided what this deployment does
// and no client could see one of them. The write half exists because seeing them is not the whole of the
// requirement: "no config should live locally, it must be pushed from the panel to the machine". The
// measurement that decides the shape is in the catalogue's own header and re-run here on the live stack
// 2026-08-01:
//
//	curl -s .../v1/deployment | jq -r '.settings[].mutability' | sort | uniq -c
//	  32 bring_up
//	   3 bring_up_default_only
//
// Thirty-two of thirty-five are read from the process environment, fixed at exec. So the panel cannot edit
// a running process and a control that claimed to would be the defect this tree keeps finding. It writes a
// DESIRED document instead; the bring-up reads it and turns it into the process's environment.
//
// The four properties, one test each:
//
//  1. EVERY SETTING WAS DECIDED ABOUT. Writable, or refused with a stated reason. No third state.
//  2. A PATH CANNOT BECOME WRITABLE. The refusal is in the set-builder, not in a message after the fact.
//  3. A WRITABLE SETTING REACHES THE PROCESS. A value the panel stores that the deployment's own compose
//     file does not pass is a form that writes to a table nobody consults — worse than the local file it
//     replaced, because the file at least worked.
//  4. AN ACCEPTED VALUE IS A VALUE THE READER PARSES. Every reader coerces silently; see the grammar's
//     header. The round trip against the REAL readers is in the composition root's own test, because that
//     is where the real readers are.

// TestEverySettingIsEitherWritableOrHasAStatedRefusal is the decision gate, in both directions.
//
// WALK -> LIST: a catalogue entry that is neither writable nor refused is a setting nobody decided about,
// and the default for a new one must be "not writable, and say why" rather than silence.
//
// LIST -> WALK: a refusal naming a setting the catalogue no longer has is a stale sentence that will be
// read as covering whatever comes back under that name. This direction is the one a walk cannot find.
func TestEverySettingIsEitherWritableOrHasAStatedRefusal(t *testing.T) {
	writable := desiredWritable()
	catalogued := map[string]bool{}

	for _, entry := range deploymentCatalogue {
		catalogued[entry.Name] = true
		_, isWritable := writable[entry.Name]
		reason, isRefused := nonDesiredReason[entry.Name]
		switch {
		case isWritable && isRefused:
			t.Errorf("%s is BOTH writable and refused. Two sources of truth about one setting is how a refusal "+
				"survives the change that made it writable", entry.Name)
		case !isWritable && !isRefused:
			t.Errorf("%s is neither writable from the panel nor listed in nonDesiredReason. A setting nobody "+
				"decided about is not read-only by design, it is read-only by accident — give it a DesiredValue "+
				"grammar or a sentence saying why an operator cannot set it here", entry.Name)
		case isRefused && strings.TrimSpace(reason) == "":
			t.Errorf("nonDesiredReason[%s] is empty. An exemption nobody has to justify is a place to hide a setting", entry.Name)
		}
	}

	for name := range nonDesiredReason {
		if !catalogued[name] {
			t.Errorf("nonDesiredReason names %s, which the catalogue no longer carries. A refusal for a setting "+
				"that does not exist is a sentence waiting to be read as covering whatever comes back under that name", name)
		}
	}
}

// TestAPathSettingCanNeverBeWritable is requirement 3's structural leg, and it is written so that it proves
// the refusal is STRUCTURAL rather than checking that somebody remembered.
//
// Two assertions, and the second is the one that matters:
//
//   - No path-kind entry is in the writable set today.
//   - A path-kind entry that DECLARES a value grammar — the exact edit a future author would make to open
//     one up — is still refused, because desiredWritable filters on Kind before it looks at DesiredValue.
//
// The second is run against a real catalogue entry, mutated in place and restored, so it exercises the
// shipped function rather than a copy of its logic.
func TestAPathSettingCanNeverBeWritable(t *testing.T) {
	for _, entry := range deploymentCatalogue {
		if entry.Kind != kindPath {
			continue
		}
		if _, ok := desiredWritable()[entry.Name]; ok {
			t.Errorf("%s is a filesystem path and it is writable from the panel. A *_FILE value is a handle to a "+
				"credential; the read surface reports the path and says 'never the contents', and a write surface "+
				"that moved it would point the process at a file the caller chose", entry.Name)
		}
	}

	// The perturbation. Pick the sharpest path in the catalogue and give it a grammar, which is exactly what
	// an author opening it up would type. It must STILL be refused.
	const victim = "PALAI_SECRET_MASTER_KEY_FILE"
	index := -1
	for i := range deploymentCatalogue {
		if deploymentCatalogue[i].Name == victim {
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("%s is no longer catalogued; this test's perturbation has nothing to perturb", victim)
	}
	restore := deploymentCatalogue[index].DesiredValue
	deploymentCatalogue[index].DesiredValue = desiredToken
	defer func() { deploymentCatalogue[index].DesiredValue = restore }()

	if _, ok := desiredWritable()[victim]; ok {
		t.Fatalf("declaring a value grammar on %s made it writable. The refusal is a validation string, not a "+
			"structure: desiredWritable must drop Kind == kindPath BEFORE it consults the grammar, so that opening "+
			"a path takes deleting the filter rather than editing a row", victim)
	}
	if row, ok := desiredWritable()["PALAI_DISPATCH_WORKERS"]; !ok || row.DesiredValue != desiredInt {
		t.Fatal("the perturbation left the writable set empty or wrong, so the assertion above passed vacuously")
	}
}

// TestEveryWritableSettingIsPassedByTheShippedComposeFile is requirement 3 — the one that decides whether
// any of this does anything at all.
//
// A desired document is applied by `palai up` EXPORTING the value into the environment it drives
// `docker compose` with. compose.yaml then has to hand it to the container, and it does that only for the
// keys it names with `${VAR}`. A key compose does not name is a key the container never sees: the panel
// stores it, the CLI exports it, the screen reports the write succeeded, and the process runs without it.
//
// THAT IS NOT HYPOTHETICAL AND THIS TEST FOUND IT. Measured on main at 444a1576, 2026-08-01:
//
//	for v in PALAI_QUEUE_DEADLINE PALAI_REQUEST_RATE_PER_SEC PALAI_REQUEST_BURST \
//	         PALAI_MAX_CONCURRENT_RUNS PALAI_MAX_QUEUED_RUNS; do
//	  grep -c "$v" deploy/compose/compose.yaml deploy/compose/production.yml
//	done
//	-> 0 for all five, in both files
//
//	docker inspect palai-a1e00dad-control-plane-1 --format '{{range .Config.Env}}{{println .}}{{end}}' \
//	  | grep -c 'PALAI_MAX_CONCURRENT_RUNS\|PALAI_QUEUE_DEADLINE\|...'   -> 0
//
// main.go reads all five (edgeLimitsFromEnv at 1969, SetQueueDeadline at 604) and no compose file has ever
// passed one. So every compose deployment ships with unbounded per-key request rate, unbounded burst,
// unbounded concurrent and queued runs per tenant, and no queue deadline — and the catalogue's own
// change_with column told operators to "recreate the control-plane with the new value", which for those
// five is a command that cannot work because there is no value to recreate with.
// THE BASE FILE IS THE ONE ASSERTED AGAINST, and that is measured rather than assumed. production.yml is an
// OVERLAY (`docker compose -f compose.yaml -f production.yml`) whose control-plane `environment:` block
// re-declares two keys, so a walk of it in isolation finds one interpolation and would fail this guard
// while the deployment it describes is correct. That merge was checked rather than believed, 2026-08-01:
//
//	PALAI_MODEL=probe-model-value docker compose -f compose.yaml -f production.yml config | grep PALAI_MODEL
//	  PALAI_MODEL: probe-model-value          <- inherited from the base; production.yml never names it
//	  PALAI_MODEL_PROVIDER: fake
//
// So the base carries the contract and the overlay's own risk is the opposite one — pinning a LITERAL over
// an interpolated base key, which would silently make that setting unwritable in production only. That is
// the second loop.
func TestEveryWritableSettingIsPassedByTheShippedComposeFile(t *testing.T) {
	const base = "deploy/compose/compose.yaml"
	passed := composeInterpolatedNames(t, base)
	if len(passed) < 8 {
		t.Fatalf("%s: the interpolation walk found %d ${PALAI_*} references, which is too few to be a parse of this "+
			"file — every assertion below it would be vacuous", base, len(passed))
	}
	var missing []string
	for _, name := range DesiredWritableSettings() {
		if !passed[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s never passes %d writable setting(s) to the control-plane container: %s\n"+
			"The panel can store a value for each of them and the process will never see it. A form that writes "+
			"to a table nobody consults is worse than the local file it replaced — the file at least worked.",
			base, len(missing), strings.Join(missing, ", "))
	}

	// The overlay's own failure mode: a key it re-declares with a LITERAL overrides the base's ${...} and
	// makes that setting silently unwritable on exactly the deployments that matter most.
	const overlay = "deploy/compose/production.yml"
	interpolated := composeInterpolatedNames(t, overlay)
	for _, name := range overlayDeclaredNames(t, overlay) {
		if _, writable := desiredWritable()[name]; !writable {
			continue
		}
		if !interpolated[name] {
			t.Errorf("%s re-declares %s with a literal value. The overlay wins over the base, so a production "+
				"deployment would pin that setting and the desired document could never move it — while the "+
				"screen reported the write succeeded", overlay, name)
		}
	}
}

// overlayDeclaredNames returns the PALAI_* keys a compose file's service environment blocks SET, whatever
// their value. It is the KEY walk (not the interpolation walk) because the overlay question is "which keys
// does this file take over", and a key taken over with a literal is precisely the failure.
func overlayDeclaredNames(t *testing.T, relative string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootFromTest(t), relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", relative, err)
	}
	var names []string
	for _, svc := range doc.Services {
		for key := range svc.Environment {
			if strings.HasPrefix(key, "PALAI_") {
				names = append(names, key)
			}
		}
	}
	sort.Strings(names)
	return names
}

// composeInterpolatedNames returns every PALAI_* variable a compose file passes to a service by
// INTERPOLATION — `KEY: ${PALAI_X}` or `${PALAI_X:-default}`.
//
// IT IS THE INTERPOLATED SET AND NOT THE KEY SET, and that distinction is the whole measurement. compose.yaml
// writes `PALAI_S3_BUCKET: palai-artifacts` — a key that is SET but carries a literal, so exporting
// PALAI_S3_BUCKET in the shell that runs compose changes nothing. Only a `${...}` value reads the
// environment `palai up` builds. deployment_test.go's composeSettingNames walks the KEYS, which is the right
// question for "is this setting catalogued"; this is the right question for "can a value get in".
func composeInterpolatedNames(t *testing.T, relative string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootFromTest(t), relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", relative, err)
	}
	out := map[string]bool{}
	for _, svc := range doc.Services {
		for key, value := range svc.Environment {
			if !strings.HasPrefix(key, "PALAI_") {
				continue
			}
			// The variable the VALUE interpolates, which is the only one an exported value can reach. It is
			// almost always the key's own name; reading it out of the value rather than assuming so is what
			// makes a literal like `PALAI_S3_BUCKET: palai-artifacts` correctly report as not-interpolated.
			for _, ref := range interpolationPattern.FindAllStringSubmatch(value, -1) {
				out[ref[1]] = true
			}
		}
	}
	return out
}

// TestDesiredWriteRefusesEveryHostileBody is requirement 3's behavioural leg — the structural one above
// proves a path cannot ENTER the writable set; this one proves the decoder refuses everything else that
// tries to reach it.
//
// The cases are not invented. Each one is a shape this tree has actually shipped, or one the value's
// journey makes reachable: the journey is JSON body -> JSONB document -> `palai up` -> os.Setenv -> a
// `docker compose` environment -> `${VAR}` interpolation into a YAML scalar.
func TestDesiredWriteRefusesEveryHostileBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // a substring the refusal must carry, so the message is asserted and not just the error
	}{
		{"a path smuggled in by name", `{"settings":{"PALAI_SECRET_MASTER_KEY_FILE":"/tmp/mine"}}`,
			"not writable from this surface"},
		{"the shell posture", `{"settings":{"PALAI_SHELL_NATIVE":"unsandboxed-host"}}`,
			"DELETES a security boundary"},
		{"the object store a credential is sent to", `{"settings":{"PALAI_S3_ENDPOINT":"https://attacker.example"}}`,
			"credential exfiltration"},
		{"an image reference", `{"settings":{"PALAI_ENGINE_IMAGE":"evil@sha256:aa"}}`,
			"arbitrary container execution"},
		{"the address this API is reached on", `{"settings":{"PALAI_LISTEN_ADDR":"127.0.0.1:1"}}`,
			"cannot be reached to change it back"},
		{"a variable nobody catalogued", `{"settings":{"PALAI_SECRET_PROVIDER_ONE":"sk-live-aaa"}}`,
			"not a setting this deployment declares"},
		{"a credential-bearing variable", `{"settings":{"PALAI_DATABASE_URL":"postgres://u:p@h/d"}}`,
			"not a setting this deployment declares"},
		{"an unknown top-level field", `{"settings":{},"apply":true}`, "carry no other field"},
		{"no settings field at all", `{}`, "`settings` is required"},
		{"a duration a human reads and Go does not", `{"settings":{"PALAI_SANDBOX_WALL_TIME":"10min"}}`,
			"write `10m`"},
		// THE UNITLESS ONE IS A DIFFERENT FOOTGUN FROM `10min` AND IS THE ONE AN OPERATOR TYPES. `10min` is
		// a misspelt unit; `3600` is a plausible number of SECONDS with no unit at all, which is what
		// somebody writes for "one hour". envDuration is time.ParseDuration and returns 0 on ANY error —
		// identical to unset — so accepting it would store a value that silently means `never` while the
		// screen reported the write succeeded. The grammar refuses it at the boundary instead.
		{"a duration in bare seconds, which means `never` to the reader", `{"settings":{"PALAI_FLEET_PARK_TTL":"3600"}}`,
			"missing unit"},
		{"an integer that is a word", `{"settings":{"PALAI_DISPATCH_WORKERS":"four"}}`, "strconv.Atoi"},
		{"an integer with a sign the reader normalises away", `{"settings":{"PALAI_DISPATCH_WORKERS":"+4"}}`,
			"does not survive a round trip"},
		{"an integer with leading whitespace", `{"settings":{"PALAI_DISPATCH_WORKERS":" 4"}}`, "strconv.Atoi"},
		{"a negative bound", `{"settings":{"PALAI_MAX_QUEUED_RUNS":"-1"}}`, "negative"},
		{"a rate that is not finite", `{"settings":{"PALAI_REQUEST_RATE_PER_SEC":"Inf"}}`, "finite"},
		{"a newline, which edits the YAML rather than the value", "{\"settings\":{\"PALAI_MODEL\":\"gpt-4o\\n      PALAI_SHELL_NATIVE: unsandboxed-host\"}}",
			"control character"},
		{"a second round of compose interpolation", `{"settings":{"PALAI_MODEL":"${PALAI_SECRET_PROVIDER_ONE}"}}`,
			"$ ` \\ \" '"},
		{"an empty value, which is a third state", `{"settings":{"PALAI_MODEL":""}}`, "Remove the key"},
		{"a value longer than any this deployment parses", `{"settings":{"PALAI_MODEL":"` + strings.Repeat("a", 300) + `"}}`,
			"at most 256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeDesiredSettings([]byte(tc.body))
			if err == nil {
				t.Fatalf("accepted %s and stored %v. Every one of these reaches os.Setenv and then a compose "+
					"interpolation; the body is assumed hostile", tc.body, got)
			}
			if !errors.Is(err, ErrDesiredRefused) {
				t.Errorf("refusal is not an ErrDesiredRefused: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal does not say why.\n  got:  %v\n  want it to contain: %q", err, tc.want)
			}
		})
	}
}

// TestDesiredWriteAcceptsTheValuesAnOperatorActuallyTypes is the other direction, and it is not decoration:
// a validator that refuses everything passes every test above and ships a form nobody can use.
func TestDesiredWriteAcceptsTheValuesAnOperatorActuallyTypes(t *testing.T) {
	const body = `{"settings":{
		"PALAI_DISPATCH_WORKERS":"4",
		"PALAI_QUEUE_DEADLINE":"15m",
		"PALAI_RETENTION_STORE_FALSE_TTL":"720h",
		"PALAI_SANDBOX_WALL_TIME":"1h30m",
		"PALAI_REQUEST_RATE_PER_SEC":"12.5",
		"PALAI_REQUEST_BURST":"40",
		"PALAI_MAX_CONCURRENT_RUNS":"8",
		"PALAI_MAX_QUEUED_RUNS":"100",
		"PALAI_RUNNER_CERT_TTL":"5m",
		"PALAI_MODEL_PROVIDER":"provider-one",
		"PALAI_MODEL":"gpt-4o-mini",
		"PALAI_TOOL_ERROR_BUDGET":"32"
	}}`
	got, err := DecodeDesiredSettings([]byte(body))
	if err != nil {
		t.Fatalf("refused a document naming every writable setting with a value its own reader parses: %v", err)
	}
	// CONTROL-PLANE WRITABLES, because this document is a control-plane document. The count stopped being
	// "every writable setting" the day a second plane got a reader: a runner-plane setting cannot appear in
	// this body at all — the plane check refuses it — so comparing against the whole set would make this
	// guard fail for the one reason it is not about.
	if len(got.Settings) != len(DesiredWritableSettingsFor(planeControlPlane)) {
		t.Fatalf("accepted %d of %d control-plane writable settings; this document names all of them, so any "+
			"shortfall is a setting the panel advertises and the decoder will not take",
			len(got.Settings), len(DesiredWritableSettingsFor(planeControlPlane)))
	}
	// And the narrowing must not become a hole: every writable setting belongs to one of the two planes, so
	// a name that is in neither list is a setting no guard covers.
	if a, b := len(DesiredWritableSettingsFor(planeControlPlane))+len(DesiredWritableSettingsFor(planeRunnerPool)), len(DesiredWritableSettings()); a != b {
		t.Fatalf("the per-plane lists carry %d writable settings and the whole set carries %d; a setting in "+
			"neither plane's list is one this guard silently stopped covering", a, b)
	}
	// AND IT LANDS ON THE CONTROL PLANE BY DEFAULT. A body with no `plane` is every existing caller's body,
	// and it must go where the reader is rather than nowhere.
	if got.Plane != planeControlPlane || got.Scope != "" {
		t.Errorf("a body with no plane decoded to %q/%q, want the control-plane singleton", got.Plane, got.Scope)
	}
	// The empty document is the "go back to every deployment default" operation and must be spelled, not
	// stumbled into: it is accepted here and refused above when `settings` is absent entirely.
	if _, err := DecodeDesiredSettings([]byte(`{"settings":{}}`)); err != nil {
		t.Fatalf(`{"settings":{}} is the clear-everything operation and was refused: %v`, err)
	}
}

// TestDesiredDriftComparesTheStringTheBringUpWillExport pins the comparison, and it pins the ONE case where
// the obvious alternative gets the answer backwards.
//
// PALAI_DISPATCH_WORKERS unset is ONE worker to DispatchWorkers() and ZERO to compose.yaml, which defaults
// it to 0. So a drift check comparing BEHAVIOUR would read desired="1" against an unset process as "no
// drift, nothing to do" and leave the operator on the queued-only stack this entire surface exists to
// expose. The comparison is on the raw string, because the raw string is what the next bring-up exports.
func TestDesiredDriftComparesTheStringTheBringUpWillExport(t *testing.T) {
	doc := &DesiredDocument{Settings: map[string]string{
		"PALAI_DISPATCH_WORKERS": "1",
		"PALAI_MODEL":            "gpt-4o-mini",
	}}
	unsetProcess := func(string) string { return "" }
	drifted := desiredDrift(doc, unsetProcess)
	if len(drifted) != 2 {
		t.Fatalf("drift against a process holding neither value = %v, want both. A desired PALAI_DISPATCH_WORKERS=1 "+
			"against an unset process is REAL drift: compose.yaml defaults that variable to 0, so 'unset' and '1' are "+
			"opposite deployments even though DispatchWorkers() reads them as the same number", drifted)
	}
	if drifted[0] != "PALAI_DISPATCH_WORKERS" || drifted[1] != "PALAI_MODEL" {
		t.Errorf("drift = %v, want it sorted so a screen and a CLI print the same list", drifted)
	}

	matching := func(name string) string { return doc.Settings[name] }
	if drifted := desiredDrift(doc, matching); len(drifted) != 0 {
		t.Errorf("a process holding exactly the desired values still reports drift %v — a pending banner that never "+
			"clears is the wallpaper this tree keeps deleting", drifted)
	}
	if drifted := desiredDrift(nil, unsetProcess); drifted != nil {
		t.Errorf("no desired document at all reported drift %v. A machine nobody has written a desired configuration "+
			"for is not drifted from anything, and saying it is would tell an operator the panel is in control when "+
			"the compose file still is", drifted)
	}
}

// TestEverySettingsPlaneIsTheOneItsReaderRunsIn is the SCOPING guard, and it is the answer to the question
// that produced it: "can this be configured per machine?"
//
// MEASURED ON THE LIVE STACK, 2026-08-01:
//
//	curl .../v1/deployment | jq 'keys'            -> ["object","settings","warnings"]   (no machine axis)
//	curl .../v1/runners      | jq '.data|length'  -> 3
//	curl .../v1/runner-pools | jq '.data|length'  -> 17
//
// There are three scopes in this product — the control-plane PROCESS, the POOL, and the RUNNER — and this
// catalogue describes exactly one of them. Every setting here is read by the control-plane process, which
// every project and every machine on the deployment shares. Saying so is the whole point: a screen that
// let an operator read "configuration" as "this machine's configuration" would be wrong about which
// machines a change reaches, and about how many of them there are.
//
// THE PLANE IS DERIVED FROM THE CITATION RATHER THAN TRUSTED. TestEveryCatalogueCitationResolvesToARealReader
// already parses the cited file and asserts the cited function's source mentions the variable; this asserts
// the plane agrees with WHERE that file runs. So the plane claim inherits an existing proof instead of
// becoming a third field to keep in step by hand — and a citation into a directory neither list knows
// about is a REFUSAL, not a default, so a new reader location asks somebody to decide.
func TestEverySettingsPlaneIsTheOneItsReaderRunsIn(t *testing.T) {
	planes := map[string]int{}
	for _, entry := range deploymentCatalogue {
		derived, ok := planeMatchesReader(entry)
		if derived == "" {
			t.Errorf("%s cites %s, which is in neither controlPlaneReaderFiles nor runnerReaderFiles. The plane "+
				"decides which PROCESS a desired value reaches, so a citation nobody has placed must be a decision "+
				"rather than a default", entry.Name, entry.ReaderFile)
			continue
		}
		if !ok {
			t.Errorf("%s declares plane %q and its reader %s runs on the %s plane. A setting written into the "+
				"wrong plane's document is a value the process that reads it never sees",
				entry.Name, planeOf(entry), entry.ReaderFile, derived)
		}
		planes[derived]++
	}

	// BOTH PLANES ARE PRESENT, and the catalogue would be less honest with only one. PALAI_RUNNER_POSTURE
	// and PALAI_RUNNER_POOL are read by cmd/runner and set by NO shipped compose file, so the compose walk —
	// which can only see what compose sets — reported a complete catalogue while two variables the runner
	// binary reads were in it nowhere. A walk finds what exists; only a list finds what does not.
	if planes[planeRunnerPool] == 0 {
		t.Error("no catalogue entry is on the runner plane, so every assertion about plane handling below is " +
			"vacuous — and the two settings cmd/runner reads and compose sets nowhere are uncatalogued again")
	}
	if planes[planeControlPlane]+planes[planeRunnerPool] != len(deploymentCatalogue) {
		t.Errorf("%d + %d planes ≠ %d entries", planes[planeControlPlane], planes[planeRunnerPool], len(deploymentCatalogue))
	}
}

// TestARunnerPlaneRowNeverReportsThisProcessesCopyOfIt is the property that REPLACED "no runner-plane
// entry exists", and it is the one that matters.
//
// Cataloguing a runner-scoped setting creates the exact trap deployment.go's header refuses for
// PALAI_RUNNER_CONCURRENCY: the handler does os.LookupEnv on every catalogued name, so a runner variable
// that happens to be exported in the CONTROL PLANE's own shell would be read, reported, and taken by a
// reader for the machine's value. "A confident wrong answer, which is worse than the silence this surface
// replaces."
//
// So the handler does not look it up at all, and this drives the SHIPPED router with the variable set to a
// sentinel to prove it. `observable:false` is what the row carries instead — which a screen can render as
// "this is read on the machines, not here", distinct from "— unset", because those are opposite facts and
// the empty string is the same for both.
func TestARunnerPlaneRowNeverReportsThisProcessesCopyOfIt(t *testing.T) {
	var runnerPlane []catalogueEntry
	for _, entry := range deploymentCatalogue {
		if planeOf(entry) == planeRunnerPool {
			runnerPlane = append(runnerPlane, entry)
		}
	}
	if len(runnerPlane) == 0 {
		t.Fatal("no runner-plane entry to test; this guard would pass vacuously")
	}
	// The trap, set deliberately: the control-plane process holds a value for a variable it does not read.
	for _, entry := range runnerPlane {
		t.Setenv(entry.Name, "PALAI-WRONG-PLANE-SENTINEL")
	}
	body, raw := deploymentBodyOf(t, bareRouter())

	if strings.Contains(raw, "PALAI-WRONG-PLANE-SENTINEL") {
		t.Error("this process's own copy of a RUNNER-scoped variable reached the response body. It would be read " +
			"as the machine's value, and it is not — this process is not the reader")
	}
	for _, entry := range runnerPlane {
		row, ok := settingNamed(body, entry.Name)
		if !ok {
			t.Errorf("%s is catalogued and not reported. It is in the catalogue precisely because no compose file "+
				"sets it and the compose walk could never find it", entry.Name)
			continue
		}
		if row.Observable {
			t.Errorf("%s is reported as observable by a process that does not read it", entry.Name)
		}
		if row.Value != "" || row.Set {
			t.Errorf("%s reports value=%q set=%v; this process holds no copy of a runner-scoped variable", entry.Name, row.Value, row.Set)
		}
		if row.Plane != planeRunnerPool {
			t.Errorf("%s reports plane %q on the wire, so a screen cannot tell it apart from an unset local setting", entry.Name, row.Plane)
		}
		// WRITABLE IFF IT IS CONFIGURATION RATHER THAN IDENTITY, and the distinction is the reason this is
		// not simply "the runner plane is writable now". PALAI_RUNNER_CONCURRENCY is a decision an operator
		// makes about a fleet — four sessions on this Mac's pool — and it now reaches the machine on its
		// enrolment answer. PALAI_RUNNER_POOL and PALAI_RUNNER_POSTURE are what a machine IS: the pool comes
		// from the credential it presented and the posture from the box it is. A panel that could write
		// either would be able to move a machine between pools, or to relabel an unsandboxed host as a
		// sandboxed one, by editing a document — and the enrolment answer is delivered by the very pool
		// lookup that would then be reading a value the panel supplied.
		//
		// The catalogue already encodes it: a setting is writable exactly when it declares a DesiredValue
		// grammar, so this asserts the two agree rather than restating the list.
		if want := entry.DesiredValue != ""; row.Writable != want {
			t.Errorf("%s reports writable=%v, want %v — a runner-plane setting is writable exactly when it is "+
				"configuration (it declares a desired grammar) and never when it is the machine's identity",
				entry.Name, row.Writable, want)
		}
		if row.Default == "" {
			t.Errorf("%s carries no default. Its VALUE is not what a reader wants — its existence and what a runner "+
				"without it falls back to are, and that is the whole reason to catalogue an unobservable setting", entry.Name)
		}
	}

	// AND A CONTROL-PLANE ROW IS STILL OBSERVABLE, or the skip above is skipping everything.
	if row, _ := settingNamed(body, "PALAI_DISPATCH_WORKERS"); !row.Observable {
		t.Error("a control-plane setting reports observable=false; the plane skip is disabling the whole surface")
	}
}

// TestThePlaneGuardCanTellTheTwoPlanesApart is the anti-vacuity leg, and it is not decoration.
//
// The guard above asserts that thirty-five entries all sit on one plane. With only one list of prefixes
// that assertion is unfalsifiable — it would be checking that everything is what everything is, and it
// would pass on a catalogue whose runner-plane entries had simply been mislabelled. So this drives the
// SHIPPED function with a citation into the runner binary and requires it to come back with the other
// answer, and to REFUSE a citation it has never been told about.
func TestThePlaneGuardCanTellTheTwoPlanesApart(t *testing.T) {
	// The real one: cmd/runner/main.go:117 reads PALAI_RUNNER_CONCURRENCY through envIntDefault. It is the
	// one genuinely per-machine knob in this product and it is deliberately NOT catalogued — see
	// unreportedSettings, which says this process holds no copy.
	runner := catalogueEntry{Name: "PALAI_RUNNER_CONCURRENCY", ReaderFile: "cmd/runner/main.go", ReaderFunc: "main", Plane: planeRunnerPool}
	if derived, ok := planeMatchesReader(runner); derived != planeRunnerPool || !ok {
		t.Errorf("a citation into cmd/runner derived plane %q ok=%v, want %q/true. If the guard cannot recognise "+
			"the runner plane, its assertion that nothing is on it is vacuous", derived, ok, planeRunnerPool)
	}
	// And the same entry MISLABELLED as control-plane must fail, which is the failure the guard exists for.
	mislabelled := runner
	mislabelled.Plane = planeControlPlane
	if _, ok := planeMatchesReader(mislabelled); ok {
		t.Error("a runner-read setting labelled control_plane was accepted. That is a value written into the " +
			"control plane's document that the machine reading it never sees")
	}
	// A citation nobody has placed REFUSES rather than defaulting.
	if derived, ok := planeMatchesReader(catalogueEntry{Name: "X", ReaderFile: "engines/reference/main.go"}); derived != "" || ok {
		t.Errorf("an unplaced citation derived %q ok=%v, want \"\"/false — a default here would silently call a "+
			"new reader location the control plane", derived, ok)
	}
}

// TestARunnerPlaneWriteIsRefusedByNameRatherThanStored is the scoping decision made testable.
//
// THE PLANE NOW HAS A READER, AND THIS TEST IS THE RECORD OF WHAT THAT CHANGED. It used to assert the
// opposite — that a runner_pool write was REFUSED — and the refusal's own sentence said why: "nothing hands
// cmd/runner a document: it reads its environment at exec, and the second binary that would read one is not
// this task." That binary now reads one. RunnerGateway.handleEnroll answers an enrolling machine its pool's
// desired document and cmd/runner takes its lease concurrency from that answer, so a stored row changes a
// machine instead of being a save nobody consults.
//
// WHAT SURVIVES THE FLIP IS THE SCOPE. A control-plane document is a singleton and takes no scope_id; a
// runner_pool document configures ONE pool out of the seventeen the live stack returned, so a scope is the
// difference between configuring a fleet and configuring nothing in particular. Migration 000053's CHECK
// enforces it too, but a constraint violation reaches an operator as a 500 — this reaches them as a
// sentence naming what they left out.
func TestARunnerPlaneWriteIsAcceptedNowThatAMachineReadsIt(t *testing.T) {
	got, err := DecodeDesiredSettings([]byte(`{"plane":"runner_pool","scope_id":"pool_default","settings":{"PALAI_RUNNER_CONCURRENCY":"4"}}`))
	if err != nil {
		t.Fatalf("a runner_pool document was refused: %v. The enrolment answer carries it to the machine now, "+
			"so refusing it leaves an operator unable to configure a fleet from the panel", err)
	}
	if got.Plane != planeRunnerPool || got.Scope != "pool_default" {
		t.Fatalf("decoded plane=%q scope=%q, want the runner plane scoped to the pool that was named", got.Plane, got.Scope)
	}
	if got.Settings["PALAI_RUNNER_CONCURRENCY"] != "4" {
		t.Fatalf("decoded settings %v, want the value the operator typed", got.Settings)
	}
}

// A pool document with no pool is refused BY NAME rather than by the database's CHECK constraint, so the
// operator reads a sentence instead of a 500.
func TestARunnerPlaneWriteWithoutAScopeIsRefusedByName(t *testing.T) {
	_, err := DecodeDesiredSettings([]byte(`{"plane":"runner_pool","settings":{"PALAI_RUNNER_CONCURRENCY":"4"}}`))
	if err == nil {
		t.Fatal("a scopeless runner_pool document was accepted; migration 000053's CHECK would then refuse it as " +
			"a 500, which tells an operator nothing about what they left out")
	}
	for _, want := range []string{"scope_id", "pool"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A control-plane document must still not carry a runner-plane setting: the value would be exported into a
// process that never reads it, which is the defect the plane check exists for.
func TestAControlPlaneDocumentRefusesARunnerPlaneSetting(t *testing.T) {
	_, err := DecodeDesiredSettings([]byte(`{"settings":{"PALAI_RUNNER_CONCURRENCY":"4"}}`))
	if err == nil {
		t.Fatal("the control-plane document accepted a runner-plane setting; it would be exported into a process " +
			"that never reads it while the screen reported a save")
	}
}

func TestASettingCannotBeWrittenIntoAPlaneItIsNotReadOn(t *testing.T) {
	const victim = "PALAI_DISPATCH_WORKERS"
	index := -1
	for i := range deploymentCatalogue {
		if deploymentCatalogue[i].Name == victim {
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("%s is no longer catalogued", victim)
	}
	restore := deploymentCatalogue[index].Plane
	deploymentCatalogue[index].Plane = planeRunnerPool
	defer func() { deploymentCatalogue[index].Plane = restore }()

	_, err := DecodeDesiredSettings([]byte(`{"settings":{"` + victim + `":"4"}}`))
	if err == nil {
		t.Fatalf("a control-plane document accepted %s while it is read on the runner plane. The value would be "+
			"exported into a process that never reads it, and the screen would report a change that reached nothing", victim)
	}
	if !strings.Contains(err.Error(), "wrong plane") {
		t.Errorf("the refusal does not say the plane is the problem: %v", err)
	}
}

// TestTurningWorkspacesOnWarnsAboutTheOtherPlanesCopy is the screen's half of the two-reader finding.
//
// PALAI_WORKSPACE_ROOT is read by this process (allocate workspaces here) and by every runner (refuse to
// bind-mount a leased path outside here). Setting it HERE turns the feature on; the boundary that guards
// the feature lives THERE. Measured 2026-08-01: no shipped file gave a runner its copy, and an unset root
// used to disable the runner's check entirely — so the deployment that made workspaces work was exactly
// the deployment whose boundary was off.
//
// THE WARNING CLAIMS NOTHING ABOUT THE RUNNER'S STATE, and that restraint is the test. This process holds
// no copy of a runner-scoped variable; a surface that reported one would be doing the thing deployment.go
// refuses to do for PALAI_RUNNER_CONCURRENCY — "a confident wrong answer, which is worse than the silence
// this surface replaces". So the assertion is that it names both readers and the remedy, and that it says
// so without asserting the other plane is misconfigured.
func TestTurningWorkspacesOnWarnsAboutTheOtherPlanesCopy(t *testing.T) {
	t.Setenv("PALAI_WORKSPACE_ROOT", "/srv/palai/workspaces")
	body, _ := deploymentBodyOf(t, bareRouter())

	warn, ok := warningCoded(body, warnWorkspaceRootPlane)
	if !ok {
		t.Fatalf("workspaces are provisioned and nothing said the machines that mount them need the same variable. "+
			"warnings = %+v", body.Warnings)
	}
	if warn.Severity != severityAdvisory {
		t.Errorf("severity = %q, want %q: the deployment works, and what is uncertain is a boundary on a plane this "+
			"process cannot read — a blocking band would be a red screen on a healthy stack", warn.Severity, severityAdvisory)
	}
	for _, want := range []string{"PALAI_WORKSPACE_ROOT", "cmd/runner", "bind-mount"} {
		if !strings.Contains(warn.Detail, want) {
			t.Errorf("the detail does not name %q — an operator cannot act on a warning that does not say which "+
				"two things are involved:\n  %s", want, warn.Detail)
		}
	}
	if !strings.Contains(warn.Remedy, "SAME host path") {
		t.Errorf("the remedy does not say the two values must match. A runner checking against a DIFFERENT root "+
			"refuses every coding run, which is a stack that looks configured and works for nothing:\n  %s", warn.Remedy)
	}
	// IT MUST NOT CLAIM THE RUNNER IS MISCONFIGURED. This process cannot see a runner's environment, and the
	// catalogue refuses to report PALAI_RUNNER_CONCURRENCY for exactly that reason.
	if strings.Contains(warn.Headline, "is not set") || strings.Contains(warn.Detail, "your runners do not") {
		t.Errorf("the warning asserts a runner's state, which this process cannot observe: %s / %s", warn.Headline, warn.Detail)
	}

	// AND IT CLEARS. Workspaces off means no lease carries a path, so there is nothing on the other plane to
	// arm — a banner that never clears is the wallpaper this tree keeps deleting.
	t.Setenv("PALAI_WORKSPACE_ROOT", "")
	body, _ = deploymentBodyOf(t, bareRouter())
	if _, ok := warningCoded(body, warnWorkspaceRootPlane); ok {
		t.Error("the warning still fires with workspaces off; no lease can carry a workspace path, so there is " +
			"nothing for a runner to place or refuse")
	}
}

// TestTheCapacityParkTTLIsSettableFromThePanel is the row this catalogue was missing, and the reason it
// matters is not coverage — it is that PALAI_FLEET_PARK_TTL was settable ONLY by editing a machine's
// environment, which is the thing the desired-config surface exists to abolish.
//
// It is the reaper for E24 T4's FLT-P7: a run parked for want of a machine waits FOREVER, because the
// only wake fires when a machine connects. Two runs on a live stack sat that way for forty-one hours on
// 2026-08-02, and the supported way to end them is this TTL — through a panel, not through a file.
//
// THE MUTABILITY IS MEASURED, NOT ASSUMED. main.go's startDispatch calls
// `WithCapacityParkTTL(envDuration("PALAI_FLEET_PARK_TTL"))` ONCE, at reconciler construction; the
// reconciler stores it in a field and Sweep reads the FIELD every tick, never the environment again. So a
// new value needs the process replaced — `bring_up`, and not `bring_up_default_only`, because there is no
// runtime write-path for a park TTL that the environment value would merely be the fallback for. A row
// claiming a restart is unnecessary would be the same class of lie as one claiming it is.
func TestTheCapacityParkTTLIsSettableFromThePanel(t *testing.T) {
	const name = "PALAI_FLEET_PARK_TTL"
	var entry catalogueEntry
	for _, e := range deploymentCatalogue {
		if e.Name == name {
			entry = e
			break
		}
	}
	if entry.Name == "" {
		t.Fatalf("%s is not in the catalogue, so the ONLY way to set it is on the machine — which is exactly what this surface exists to replace. It is the reaper that ends a run parked for want of a machine (E24 T5); without a row, a capacity policy stays a file edit", name)
	}
	if _, writable := desiredWritable()[name]; !writable {
		t.Fatalf("%s is catalogued but NOT writable: a read-only row reports the machine's value and still leaves the operator editing a file to change it", name)
	}
	if entry.Mutability != mutabilityBringUp {
		t.Fatalf("%s declares mutability %q, want %q — measured: startDispatch reads it once at reconciler construction and Sweep then reads the FIELD, never the environment", name, entry.Mutability, mutabilityBringUp)
	}
	if entry.DesiredValue != desiredDuration {
		t.Fatalf("%s declares the grammar %q, want %q — the duration grammar is what REFUSES a unitless value, and a unitless value here silently means `never`", name, entry.DesiredValue, desiredDuration)
	}

	// THE TWO CONDITIONS THAT MAKE IT A NO-OP MUST BE IN THE ROW AN OPERATOR READS. Unlike every other
	// execution setting, this one does nothing at all on a stack that dispatches nothing: the reconciler is
	// constructed BELOW startDispatch's early return, so PALAI_DISPATCH_WORKERS=0 builds no reconciler and
	// runs no sweep — and since the dispatch refusal landed, a control plane with no runner listener takes
	// that same early exit. A panel that accepts a TTL on such a stack, reports success, and expires
	// nothing is a form that lies about what it did.
	if !strings.Contains(entry.Effect, "PALAI_DISPATCH_WORKERS") {
		t.Fatalf("%s's effect text does not name PALAI_DISPATCH_WORKERS: on a stack with no dispatch workers the reconciler is never built, so this setting is accepted and expires nothing — the row must say so where it is read, not leave it to be discovered.\ngot: %s", name, entry.Effect)
	}
}
