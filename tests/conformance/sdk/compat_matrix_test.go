// E16 T7 — the honest-matrix guard. The compatibility matrix (docs/operations/sdk-compatibility.json)
// claims a cell "supported" ONLY if a real conformance test proves it. This guard makes that
// mechanical: it RECOMPUTES each SDK's covered capabilities by running that SDK's conformance runner
// against the shared corpus (the SAME machinery as harness_test.go, reused here), and asserts the
// JSON's claimed set EQUALS the recomputed set — no claimed-but-untested cell, no tested-but-omitted
// cell. A hand-edit that adds a false "supported" entry turns this RED (TestSDKCompatibilityMatrixGuardBites
// pins that the comparison actually bites).
//
// It also grounds the COLUMNS: the matrix's capability axis must equal the corpus categories on disk,
// and each SDK's proof_test reference must resolve to a real test in this package.
package sdkconf_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type compatMatrix struct {
	APIVersion   string   `json:"api_version"`
	Capabilities []string `json:"capabilities"`
	SDKs         map[string]struct {
		Version   string   `json:"version"`
		ProofTest string   `json:"proof_test"`
		Supported []string `json:"supported"`
	} `json:"sdks"`
}

func loadMatrix(t *testing.T) compatMatrix {
	t.Helper()
	p := filepath.Join("..", "..", "..", "docs", "operations", "sdk-compatibility.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read compatibility matrix: %v", err)
	}
	var m compatMatrix
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode compatibility matrix: %v", err)
	}
	return m
}

func asSet(xs []string) map[string]bool {
	s := make(map[string]bool, len(xs))
	for _, x := range xs {
		s[x] = true
	}
	return s
}

func sortedKeys(s map[string]bool) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runnerArgv returns the invocation for an SDK's conformance runner (the SAME argv the per-language
// harness tests use). It skips the SDK if its toolchain is absent AND the matching opt-out env is set.
func runnerArgv(t *testing.T, sdk string) []string {
	t.Helper()
	switch sdk {
	case "typescript":
		node, err := exec.LookPath("node")
		if err != nil {
			if os.Getenv("PALAI_SDK_CONFORMANCE_ALLOW_NO_NODE") != "" {
				t.Skipf("node absent + PALAI_SDK_CONFORMANCE_ALLOW_NO_NODE set; TS matrix leg skipped")
			}
			t.Fatalf("node not on PATH (set PALAI_SDK_CONFORMANCE_ALLOW_NO_NODE=1 to opt out): %v", err)
		}
		p, err := filepath.Abs(filepath.Join("..", "..", "..", "sdks", "typescript", "test", "conformance-runner.ts"))
		if err != nil {
			t.Fatalf("resolve TS runner: %v", err)
		}
		return []string{node, "--experimental-strip-types", p}
	case "python":
		uv, err := exec.LookPath("uv")
		if err != nil {
			if os.Getenv("PALAI_SDK_CONFORMANCE_ALLOW_NO_UV") != "" {
				t.Skipf("uv absent + PALAI_SDK_CONFORMANCE_ALLOW_NO_UV set; Python matrix leg skipped")
			}
			t.Fatalf("uv not on PATH (set PALAI_SDK_CONFORMANCE_ALLOW_NO_UV=1 to opt out): %v", err)
		}
		dir, err := filepath.Abs(filepath.Join("..", "..", "..", "sdks", "python"))
		if err != nil {
			t.Fatalf("resolve python dir: %v", err)
		}
		return []string{uv, "run", "--locked", "--project", dir, "python", filepath.Join(dir, "conformance", "runner.py")}
	case "go":
		goBin, err := exec.LookPath("go")
		if err != nil {
			t.Fatalf("go not on PATH: %v", err)
		}
		dir, err := filepath.Abs(filepath.Join("..", "..", "..", "sdks", "go"))
		if err != nil {
			t.Fatalf("resolve sdks/go: %v", err)
		}
		binary := filepath.Join(t.TempDir(), "palai-go-conformance-runner")
		build := exec.Command(goBin, "build", "-o", binary, "./runner")
		build.Dir = dir
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build Go runner: %v\n%s", err, out)
		}
		return []string{binary}
	default:
		t.Fatalf("unknown sdk %q", sdk)
		return nil
	}
}

// actualCoverage runs the SDK's runner over the full corpus and returns the set of categories the
// runner FULLY covers (output present AND matching expected for EVERY vector in the category). This
// is the ground truth the matrix's "supported" list is checked against — recomputed, not trusted.
func actualCoverage(t *testing.T, sdk string) map[string]bool {
	t.Helper()
	argv := runnerArgv(t, sdk)

	var all []runnerVector
	byKey := map[string]vector{}
	total := map[string]int{}
	for _, category := range categories {
		for _, v := range loadCategory(t, category) {
			all = append(all, runnerVector{Category: category, Name: v.Name, Input: v.Input})
			byKey[category+"\x00"+v.Name] = v
			total[category]++
		}
	}
	outputs := runExternalRunner(t, argv, all)

	matched := map[string]int{}
	for key, v := range byKey {
		category := strings.SplitN(key, "\x00", 2)[0]
		got, ok := outputs[key]
		if !ok {
			continue
		}
		equal, _, _, err := diff(rawAny(got), v.Expected)
		if err == nil && equal {
			matched[category]++
		}
	}
	covered := map[string]bool{}
	for _, category := range categories {
		if total[category] > 0 && matched[category] == total[category] {
			covered[category] = true
		}
	}
	return covered
}

// TestSDKCompatibilityMatrixColumns grounds the matrix's capability axis + API-Version + proof refs.
func TestSDKCompatibilityMatrixColumns(t *testing.T) {
	m := loadMatrix(t)

	if m.APIVersion != "2026-07-16" {
		t.Errorf("matrix api_version %q != server API-Version 2026-07-16 (middleware/request_context.go)", m.APIVersion)
	}
	// The columns must be exactly the corpus categories — no invented capability, none dropped.
	if got, want := asSet(m.Capabilities), asSet(categories); !reflect.DeepEqual(got, want) {
		t.Errorf("matrix capabilities %v != corpus categories %v", sortedKeys(got), sortedKeys(want))
	}
	// Each SDK's proof_test must resolve to a real test in this conformance package.
	testSrc := readTestSources(t)
	for name, sdk := range m.SDKs {
		if sdk.ProofTest == "" {
			t.Errorf("sdk %q has no proof_test", name)
			continue
		}
		if !strings.Contains(testSrc, "func "+sdk.ProofTest+"(") {
			t.Errorf("sdk %q proof_test %q does not resolve to a real test in tests/conformance/sdk/", name, sdk.ProofTest)
		}
	}
}

func readTestSources(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range files {
		c, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(c)
	}
	return b.String()
}

// TestSDKCompatibilityMatrixHonest is the honest-matrix guard: for each SDK, the JSON's claimed
// "supported" set must EQUAL the recomputed coverage. A claimed-but-untested cell fails here (the
// runner won't produce matching output for it); a tested-but-omitted cell fails too (the matrix must
// be complete).
func TestSDKCompatibilityMatrixHonest(t *testing.T) {
	m := loadMatrix(t)
	if len(m.SDKs) == 0 {
		t.Fatal("matrix lists no SDKs")
	}
	for name, sdk := range m.SDKs {
		name, sdk := name, sdk
		t.Run(name, func(t *testing.T) {
			claimed := asSet(sdk.Supported)
			actual := actualCoverage(t, name)
			if !reflect.DeepEqual(claimed, actual) {
				t.Errorf("matrix for %q claims %v but the runner actually covers %v\n"+
					"  (a supported cell must map to a passing corpus vector — the matrix says only what is tested)",
					name, sortedKeys(claimed), sortedKeys(actual))
			}
		})
	}
}

// TestSDKCompatibilityMatrixGuardBites proves the guard is genuine: take the REAL recomputed coverage
// for one SDK and inject a false claim (TS "supports" signature-verify, which it does not — it ships
// no webhook verify). The same equality check the guard uses must report a mismatch. A guard that
// could not fail would rubber-stamp any matrix.
func TestSDKCompatibilityMatrixGuardBites(t *testing.T) {
	actual := actualCoverage(t, "typescript")
	if actual["signature-verify"] {
		t.Fatal("precondition broken: the TS runner is not supposed to cover signature-verify")
	}
	// The lie: claim everything TS really covers PLUS signature-verify.
	lie := asSet(sortedKeys(actual))
	lie["signature-verify"] = true
	if reflect.DeepEqual(lie, actual) {
		t.Fatal("guard is inert: a claimed-but-untested cell was not detected as a mismatch")
	}
}
