package sdkconf_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorpusGoRunnerEquality registers the Go SDK leg (E16 T4) against the SAME corpus and the SAME
// stable stdin/stdout contract — no corpus change (design invariant, plan §2). It is deliberately a
// DISTINCT file/test from the TypeScript (and T3 Python) registration so the parallel integration
// merges do not collide (merge order T3 then T4).
//
// The runner is prebuilt to a temp binary from its own module (sdks/go, a standalone go.mod that
// never imports the monorepo's internal packages), then fed the whole corpus through the shared
// runExternalRunner — "a new runner is one argv here" (README). Its normalized outputs are
// canonical-bytes-diffed against the corpus's expected output, exactly like the reference and TS
// legs. With this leg green, the three-language claim's Go third is mechanically proven: a divergent
// decode (unknown-field stripped, envelopes conflated, an error mis-mapped) FAILS here.
//
// The Go SDK exposes ALL SIX categories — including signature-verify (it ships a real webhook
// verify, unlike the browser-relay TS SDK) — so it is the first shipped runner to give that category
// a second independent implementation beyond the reference.
func TestCorpusGoRunnerEquality(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go not on PATH: the Go runner leg is required: %v", err)
	}
	moduleDir, err := filepath.Abs(filepath.Join("..", "..", "..", "sdks", "go"))
	if err != nil {
		t.Fatalf("resolve sdks/go dir: %v", err)
	}

	// Build the runner from its own module (sets the module context to sdks/go, avoiding any
	// nested-module ambiguity a bare `go run <abs path>` from the root module would hit).
	binary := filepath.Join(t.TempDir(), "palai-go-conformance-runner")
	build := exec.Command(goBin, "build", "-o", binary, "./runner")
	build.Dir = moduleDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Go runner: %v\n%s", err, out)
	}

	var all []runnerVector
	byKey := map[string]vector{}
	for _, category := range categories {
		for _, v := range loadCategory(t, category) {
			all = append(all, runnerVector{Category: category, Name: v.Name, Input: v.Input})
			byKey[category+"\x00"+v.Name] = v
		}
	}

	outputs := runExternalRunner(t, []string{binary}, all)

	covered := 0
	for key, v := range byKey {
		category := strings.SplitN(key, "\x00", 2)[0]
		got, ok := outputs[key]
		if !ok {
			t.Errorf("Go runner produced no output for %s (the Go SDK exposes every category, incl. %s)", key, category)
			continue
		}
		covered++
		equal, gotCanon, wantCanon, err := diff(rawAny(got), v.Expected)
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if !equal {
			t.Errorf("Go runner diverged for %s\n got: %s\nwant: %s", key, gotCanon, wantCanon)
		}
	}
	if covered != len(byKey) {
		t.Errorf("Go runner covered %d/%d vectors — it must cover every vector, including signature-verify", covered, len(byKey))
	}
	t.Logf("Go runner matched %d/%d vectors (all six categories, incl. signature-verify)", covered, len(byKey))
}
