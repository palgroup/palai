package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	contractgen "github.com/palgroup/palai/spikes/contracts/generated/go"
)

const largeSequence int64 = 9007199254740993

var corpusCases = []corpusCase{
	{Name: "omitted", NoteState: "missing", Status: "queued"},
	{Name: "null", NoteState: "null", Status: "running"},
	{Name: "empty", NoteState: "value", Status: "completed"},
	{Name: "unknown", NoteState: "value", Status: "future-state", HasExtra: true, HasFutureMeta: true},
}

func TestGeneratedGoCorpus(t *testing.T) {
	root := contractRoot(t)
	for _, testCase := range corpusCases {
		t.Run(testCase.Name, func(t *testing.T) {
			path := filepath.Join(root, "fixtures", testCase.Name+".json")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			fixture, err := contractgen.DecodeFixture(original)
			if err != nil {
				t.Fatalf("DecodeFixture() error = %v", err)
			}
			if fixture.Sequence != largeSequence {
				t.Errorf("sequence = %d, want %d", fixture.Sequence, largeSequence)
			}
			if fixture.Status != testCase.Status {
				t.Errorf("status = %q, want %q", fixture.Status, testCase.Status)
			}
			if got := noteState(fixture.Note); got != testCase.NoteState {
				t.Errorf("note state = %q, want %q", got, testCase.NoteState)
			}
			_, hasExtra := fixture.UnknownFields["future_top_level"]
			if hasExtra != testCase.HasExtra {
				t.Errorf("future_top_level present = %t, want %t", hasExtra, testCase.HasExtra)
			}
			_, hasFutureMeta := fixture.Metadata["future_metadata"]
			if hasFutureMeta != testCase.HasFutureMeta {
				t.Errorf("future_metadata present = %t, want %t", hasFutureMeta, testCase.HasFutureMeta)
			}
			encoded, err := fixture.Encode()
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			assertJSONSemanticEqual(t, original, encoded)
		})
	}
}

func TestGeneratedPythonAndTypeScriptCorpus(t *testing.T) {
	root := contractRoot(t)
	for _, language := range []string{"python", "typescript"} {
		t.Run(language, func(t *testing.T) {
			results, err := runGeneratedLanguage(t.Context(), root, language, corpusCases)
			if err != nil {
				t.Fatalf("runGeneratedLanguage() error = %v", err)
			}
			if len(results) != len(corpusCases) {
				t.Fatalf("result count = %d, want %d", len(results), len(corpusCases))
			}
			for index, result := range results {
				want := corpusCases[index]
				if result.Name != want.Name || result.NoteState != want.NoteState || result.Status != want.Status {
					t.Errorf("result[%d] = %+v, want %+v", index, result, want)
				}
				if result.Sequence != "9007199254740993" {
					t.Errorf("result[%d] sequence = %q", index, result.Sequence)
				}
				if result.HasExtra != want.HasExtra || result.HasFutureMeta != want.HasFutureMeta {
					t.Errorf("result[%d] unknown preservation = %+v, want extra=%t metadata=%t", index, result, want.HasExtra, want.HasFutureMeta)
				}
				original, err := os.ReadFile(filepath.Join(root, "fixtures", want.Name+".json"))
				if err != nil {
					t.Fatalf("ReadFile() error = %v", err)
				}
				assertJSONSemanticEqual(t, original, []byte(result.Encoded))
			}
		})
	}
}

func TestGeneratedLanguagesRejectInvalidCorpus(t *testing.T) {
	root := contractRoot(t)
	invalidPath := filepath.Join(root, "fixtures", "invalid.json")
	invalid, err := os.ReadFile(invalidPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if _, err := contractgen.DecodeFixture(invalid); err == nil {
		t.Fatal("generated Go codec accepted invalid corpus")
	}
	for _, language := range []string{"python", "typescript"} {
		t.Run(language, func(t *testing.T) {
			if err := requireGeneratedLanguageRejection(t.Context(), root, language, invalidPath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenAPIProjectionMatchesCanonicalSchema(t *testing.T) {
	root := contractRoot(t)
	canonicalSchema := readJSONDocument(t, filepath.Join(root, "schemas", "fixture.json"))
	openAPI32 := readJSONDocument(t, filepath.Join(root, "openapi-3.2.yaml"))
	openAPI312 := readJSONDocument(t, filepath.Join(root, "generated", "openapi-3.1.2.yaml"))
	if openAPI32["openapi"] != "3.2.0" {
		t.Fatalf("canonical OpenAPI version = %v, want 3.2.0", openAPI32["openapi"])
	}
	if openAPI312["openapi"] != "3.1.2" {
		t.Fatalf("projection OpenAPI version = %v, want 3.1.2", openAPI312["openapi"])
	}
	canonicalSchema = schemaWithoutIdentity(canonicalSchema)
	projected32, err := schemaFromOpenAPI(openAPI32)
	if err != nil {
		t.Fatalf("schemaFromOpenAPI(3.2) error = %v", err)
	}
	if !reflect.DeepEqual(projected32, canonicalSchema) {
		t.Fatalf("OpenAPI 3.2 schema differs from canonical JSON Schema")
	}
	projected312, err := schemaFromOpenAPI(openAPI312)
	if err != nil {
		t.Fatalf("schemaFromOpenAPI(3.1.2) error = %v", err)
	}
	if !reflect.DeepEqual(projected312, canonicalSchema) {
		t.Fatalf("OpenAPI 3.1.2 schema differs from canonical JSON Schema")
	}
	for _, testCase := range corpusCases {
		fixture := readJSONDocument(t, filepath.Join(root, "fixtures", testCase.Name+".json"))
		if err := validateFixtureCorpus(fixture); err != nil {
			t.Fatalf("%s fixture rejected by canonical validator: %v", testCase.Name, err)
		}
	}
	invalid := readJSONDocument(t, filepath.Join(root, "fixtures", "invalid.json"))
	if err := validateFixtureCorpus(invalid); err == nil {
		t.Fatal("canonical validator accepted invalid sequence and timestamp")
	}
}

func TestGeneratedTreeIsStable(t *testing.T) {
	root := contractRoot(t)
	temporary := t.TempDir()
	if err := generateProjection(t.Context(), root, temporary); err != nil {
		t.Fatalf("generateProjection() error = %v", err)
	}
	if err := compareDirectories(filepath.Join(root, "generated"), temporary); err != nil {
		t.Fatalf("generated tree is not stable: %v", err)
	}
}

func noteState(note contractgen.OptionalNullableString) string {
	switch {
	case !note.Present:
		return "missing"
	case note.Null:
		return "null"
	default:
		return "value"
	}
}

func readJSONDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", filepath.Base(path), err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode %s error = %v", filepath.Base(path), err)
	}
	return document
}

func assertJSONSemanticEqual(t *testing.T, first, second []byte) {
	t.Helper()
	left, err := decodeSemanticJSON(first)
	if err != nil {
		t.Fatalf("decode original JSON error = %v", err)
	}
	right, err := decodeSemanticJSON(second)
	if err != nil {
		t.Fatalf("decode encoded JSON error = %v", err)
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("JSON semantic mismatch:\noriginal=%s\nencoded=%s", first, second)
	}
}

func init() {
	if largeSequence <= 1<<53 {
		panic("large sequence fixture does not exceed JavaScript's safe integer range")
	}
}

func contractRoot(t *testing.T) string {
	t.Helper()
	root, err := locateContractRoot()
	if err != nil {
		t.Fatalf("locateContractRoot() error = %v", err)
	}
	return root
}
