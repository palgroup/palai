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
		"PALAI_MODEL":"gpt-4o-mini"
	}}`
	got, err := DecodeDesiredSettings([]byte(body))
	if err != nil {
		t.Fatalf("refused a document naming every writable setting with a value its own reader parses: %v", err)
	}
	if len(got) != len(DesiredWritableSettings()) {
		t.Fatalf("accepted %d of %d writable settings; this document names all of them, so any shortfall is a "+
			"setting the panel advertises and the decoder will not take", len(got), len(DesiredWritableSettings()))
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
