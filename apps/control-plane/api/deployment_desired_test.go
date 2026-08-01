package api

import (
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
