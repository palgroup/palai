package stack

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// THE BRING-UP READER'S OWN TESTS. The desired-configuration surface makes one claim it cannot verify
// about itself — that something on the machine reads the document and turns it into the process's
// environment — and this is where that claim is a test rather than a design note.
//
// The three properties, in the order they decide whether any of it works:
//
//  1. THE VALUES ARE EXPORTED. os.Setenv is what `docker compose` interpolates ${PALAI_X} from and what
//     native.go's nativeEnv merges over. If the document does not reach os.Environ, nothing else matters.
//  2. THE EXPORT WINS OVER THE INVOKING SHELL. A shell that once exported PALAI_DISPATCH_WORKERS=0 would
//     otherwise make the document permanently inert, and the panel would show a pending bring-up that a
//     bring-up does not clear.
//  3. A BRING-UP THAT DID NOT APPLY IT FAILS. `palai up` already refuses to report success on a stack it
//     could not prove a live round-trip against; a stack running something other than what the operator
//     saved gets the same treatment, with the settings named.

// deploymentServer stands in for a control plane serving GET /v1/deployment. It is scripted per call so a
// test can make the SECOND read differ from the first — which is the whole shape of the repair path.
func deploymentServer(t *testing.T, bodies ...string) *apiClient {
	t.Helper()
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/deployment" {
			t.Errorf("the bring-up reader asked for %s; the desired document rides GET /v1/deployment, because "+
				"desired and effective are one answer and reading them apart would let the CLI apply one against a "+
				"stale copy of the other", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("the bring-up reader sent no bearer key; the surface is provision-gated")
		}
		body := bodies[len(bodies)-1]
		if calls < len(bodies) {
			body = bodies[calls]
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return &apiClient{baseURL: ts.URL, key: "test", http: ts.Client()}
}

// desiredBody renders a GET /v1/deployment body carrying a desired document, with the named settings
// drifted. It writes the same shape the shipped handler writes, field for field.
func desiredBody(revision int64, settings map[string]string, drifted []string) string {
	type row struct {
		Name       string `json:"name"`
		Value      string `json:"value"`
		Desired    string `json:"desired"`
		DesiredSet bool   `json:"desired_set"`
		Drift      bool   `json:"drift"`
	}
	body := map[string]any{"object": "deployment", "warnings": []any{}}
	rows := []row{}
	drift := map[string]bool{}
	for _, name := range drifted {
		drift[name] = true
	}
	for name, value := range settings {
		rows = append(rows, row{Name: name, Desired: value, DesiredSet: true, Drift: drift[name]})
	}
	body["settings"] = rows
	body["desired"] = map[string]any{
		"revision": revision, "written_at": "2026-08-01T00:00:00Z", "written_by": "prin_test",
		"pending": len(drifted) > 0, "drifted": drifted,
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// TestTheBringUpExportsTheDesiredDocumentOverTheInvokingShell is properties 1 and 2 together, because
// separating them would let the second pass on a function that exports nothing.
func TestTheBringUpExportsTheDesiredDocumentOverTheInvokingShell(t *testing.T) {
	// The shell `palai up` is invoked from already holds a value — the shape of an operator who exported it
	// once, months ago, and has since started configuring the machine from the panel.
	t.Setenv("PALAI_DISPATCH_WORKERS", "0")
	t.Setenv("PALAI_MODEL", "")

	api := deploymentServer(t, desiredBody(7, map[string]string{
		"PALAI_DISPATCH_WORKERS": "4",
		"PALAI_MODEL":            "gpt-4o-mini",
	}, []string{"PALAI_DISPATCH_WORKERS", "PALAI_MODEL"}))

	env, err := api.deploymentDesired()
	if err != nil {
		t.Fatalf("read the desired document: %v", err)
	}
	if !env.present || env.revision != 7 || len(env.settings) != 2 {
		t.Fatalf("desired = %+v, want revision 7 with two settings", env)
	}

	applied, err := applyDesiredEnv(env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 2 || applied[0] != "PALAI_DISPATCH_WORKERS" {
		t.Errorf("applied = %v, want both settings in sorted order (the report prints this list)", applied)
	}
	if got := os.Getenv("PALAI_DISPATCH_WORKERS"); got != "4" {
		t.Errorf("PALAI_DISPATCH_WORKERS = %q after applying a document that asks for 4.\n"+
			"This is the whole bring-up path: compose interpolates ${PALAI_DISPATCH_WORKERS} from THIS process's "+
			"environment, and native.go's nativeEnv merges over os.Environ. A value that does not land here "+
			"reaches no control plane, and the panel would report a write that changes nothing", got)
	}
	if got := os.Getenv("PALAI_MODEL"); got != "gpt-4o-mini" {
		t.Errorf("PALAI_MODEL = %q, want the desired value", got)
	}
}

// TestNoDesiredDocumentLeavesTheBringUpExactlyAsItWas is the honesty leg. A machine nobody has configured
// from the panel must come up byte-identically to how it came up before this feature existed — and the
// two shapes that mean "no document" are a null `desired` block (a fresh install) and a 404 (a control
// plane older than this CLI).
func TestNoDesiredDocumentLeavesTheBringUpExactlyAsItWas(t *testing.T) {
	t.Setenv("PALAI_DISPATCH_WORKERS", "0")

	for _, body := range []string{`{"object":"deployment","settings":[],"warnings":[],"desired":null}`} {
		api := deploymentServer(t, body)
		env, err := api.deploymentDesired()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if env.present {
			t.Errorf("a null desired block reported present=true. A machine nobody has written a document for is "+
				"running on its compose file's defaults, and saying otherwise would tell an operator the panel is in "+
				"control when the compose file still is")
		}
		applied, err := applyDesiredEnv(env)
		if err != nil || len(applied) != 0 {
			t.Errorf("applied %v with no document; a bring-up with nothing saved must export nothing", applied)
		}
		if got := os.Getenv("PALAI_DISPATCH_WORKERS"); got != "0" {
			t.Errorf("PALAI_DISPATCH_WORKERS = %q; the invoking shell's own value must survive a bring-up with no document", got)
		}
	}
}

// TestABringUpThatCannotApplyTheDocumentRefuses is property 3, and it is the one that makes the whole
// claim falsifiable.
//
// A `palai up` that reported success while the machine ran something other than what the operator saved
// would be the "declared, and nothing happens" defect wearing this command's own report — the exact thing
// GET /v1/deployment exists to expose. Three legs: the repair works, the repair is attempted exactly once,
// and a machine that still disagrees fails with the settings named.
func TestABringUpThatCannotApplyTheDocumentRefuses(t *testing.T) {
	settings := map[string]string{"PALAI_DISPATCH_WORKERS": "4"}

	t.Run("a first-install drift is repaired by one recreate", func(t *testing.T) {
		api := deploymentServer(t,
			desiredBody(3, settings, []string{"PALAI_DISPATCH_WORKERS"}), // before the recreate
			desiredBody(3, settings, nil),                               // after it
		)
		recreates := 0
		line, err := verifyDesiredApplied(api, func() error { recreates++; return nil })
		if err != nil {
			t.Fatalf("a drift a recreate fixes must not fail the command: %v", err)
		}
		if recreates != 1 {
			t.Errorf("recreates = %d, want exactly 1", recreates)
		}
		if !strings.Contains(line, "revision 3") {
			t.Errorf("report line = %q, want it to name the revision that was applied", line)
		}
	})

	t.Run("a drift a recreate does not fix fails the bring-up", func(t *testing.T) {
		api := deploymentServer(t, desiredBody(9, settings, []string{"PALAI_DISPATCH_WORKERS"}))
		recreates := 0
		_, err := verifyDesiredApplied(api, func() error { recreates++; return nil })
		if err == nil {
			t.Fatal("`palai up` reported success on a machine that is NOT running what the operator saved. " +
				"That is the defect this whole surface exists to expose, shipped into the command that applies it")
		}
		if !errors.Is(err, errDesiredNotApplied) {
			t.Errorf("refusal is not errDesiredNotApplied: %v", err)
		}
		if !strings.Contains(err.Error(), "PALAI_DISPATCH_WORKERS") {
			t.Errorf("the refusal does not name the setting that did not take: %v", err)
		}
		if !strings.Contains(err.Error(), "compose.yaml") {
			t.Errorf("the refusal does not name the likeliest cause. A setting compose.yaml does not pass with "+
				"${...} cannot be reached by any exported value, and an operator staring at a failed bring-up "+
				"deserves that sentence: %v", err)
		}
		if recreates != 1 {
			t.Errorf("recreates = %d, want exactly 1 — a second recreate is not a repair, it is a loop", recreates)
		}
	})

	t.Run("no document is reported and is not a failure", func(t *testing.T) {
		api := deploymentServer(t, `{"object":"deployment","settings":[],"warnings":[],"desired":null}`)
		line, err := verifyDesiredApplied(api, func() error { t.Fatal("recreated the control-plane with no document to apply"); return nil })
		if err != nil {
			t.Fatalf("a machine with no desired document must bring up: %v", err)
		}
		if !strings.Contains(line, "compose file") {
			t.Errorf("report line = %q — with no document the line must say what IS in control, or a reader takes "+
				"the silence for agreement", line)
		}
	})
}
