package contracts_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// mandatoryCorpusCases are the seven round-trip fixtures every language checker
// must exercise. They pin the open-world round-trip rules (spec §20.6 → API-009:
// unknown fields and open-enum values are preserved) and the ADR-0002 hard rules
// (omitted never collapses to null; 64-bit integers stay exact).
var mandatoryCorpusCases = []string{
	"omitted", "null", "empty", "unknown-field", "unknown-enum", "rfc3339", "int-boundary",
}

type corpusDocument struct {
	Note  string          `json:"note"`
	Value json.RawMessage `json:"value"`
}

type corpusFile struct {
	Case      string           `json:"case"`
	Schema    string           `json:"schema"`
	Documents []corpusDocument `json:"documents"`
}

func corpusDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "protocols", "fixtures", "corpus")
}

func loadCorpusFile(t *testing.T, name string) corpusFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(corpusDir(t), name+".json"))
	if err != nil {
		t.Fatalf("read corpus %s: %v", name, err)
	}
	var file corpusFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("decode corpus %s: %v", name, err)
	}
	return file
}

// decodeNumberJSON decodes into an untyped tree with json.Number so integer
// literals survive exactly instead of decaying to float64 (ADR-0002).
func decodeNumberJSON(t *testing.T, raw []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode json %q: %v", raw, err)
	}
	return value
}

// TestCorpusCoversMandatoryCases proves the corpus directory holds exactly the
// seven mandatory cases and that each one carries at least one non-empty
// document.
func TestCorpusCoversMandatoryCases(t *testing.T) {
	for _, name := range mandatoryCorpusCases {
		file := loadCorpusFile(t, name)
		if file.Case != name {
			t.Errorf("corpus %s.json declares case %q", name, file.Case)
		}
		if file.Schema == "" {
			t.Errorf("corpus %s.json declares no schema", name)
		}
		if len(file.Documents) == 0 {
			t.Errorf("corpus %s.json has no documents", name)
		}
		for i, doc := range file.Documents {
			if len(bytes.TrimSpace(doc.Value)) == 0 {
				t.Errorf("corpus %s.json document %d has an empty value", name, i)
			}
		}
	}

	entries, err := os.ReadDir(corpusDir(t))
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	var got []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			got = append(got, entry.Name())
		}
	}
	sort.Strings(got)
	want := make([]string, len(mandatoryCorpusCases))
	for i, name := range mandatoryCorpusCases {
		want[i] = name + ".json"
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("corpus files = %v, want exactly %v", got, want)
	}
}

// TestCorpusRoundTripsInGo decodes and re-encodes every corpus document and
// requires the value to survive unchanged: omitted fields never materialize as
// null, explicit nulls never vanish, unknown fields and open-enum values are
// preserved, and 64-bit integers stay byte-exact.
func TestCorpusRoundTripsInGo(t *testing.T) {
	for _, name := range mandatoryCorpusCases {
		file := loadCorpusFile(t, name)
		for i, doc := range file.Documents {
			before := decodeNumberJSON(t, doc.Value)
			encoded, err := json.Marshal(before)
			if err != nil {
				t.Fatalf("corpus %s.json document %d (%s): encode: %v", name, i, doc.Note, err)
			}
			after := decodeNumberJSON(t, encoded)
			if !reflect.DeepEqual(before, after) {
				t.Errorf("corpus %s.json document %d (%s) is not round-trip stable\n before=%#v\n after =%#v",
					name, i, doc.Note, before, after)
			}
			if name == "int-boundary" {
				for _, literal := range []string{"2147483647", "9007199254740991"} {
					if bytes.Contains(doc.Value, []byte(literal)) && !bytes.Contains(encoded, []byte(literal)) {
						t.Errorf("int-boundary document %d (%s) lost integer literal %s: %s", i, doc.Note, literal, encoded)
					}
				}
			}
		}
	}

	// The omitted/null pair is the headline ADR-0002 contrast: the same field is
	// absent in one file and explicitly null in the other, and each state must
	// survive its round-trip.
	requireFieldState(t, "omitted", "error", stateAbsent)
	requireFieldState(t, "null", "error", stateNull)
}

type fieldState int

const (
	stateAbsent fieldState = iota
	stateNull
)

// requireFieldState asserts at least one document in the named corpus round-trips
// with field in the requested state, proving omitted and null stay distinct.
func requireFieldState(t *testing.T, name, field string, want fieldState) {
	t.Helper()
	file := loadCorpusFile(t, name)
	for _, doc := range file.Documents {
		encoded, err := json.Marshal(decodeNumberJSON(t, doc.Value))
		if err != nil {
			t.Fatalf("corpus %s.json (%s): encode: %v", name, doc.Note, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			continue // non-object documents cannot carry the field
		}
		raw, present := fields[field]
		switch want {
		case stateAbsent:
			if !present {
				return
			}
		case stateNull:
			if present && string(raw) == "null" {
				return
			}
		}
	}
	t.Errorf("corpus %s.json has no document whose %q field round-trips in the required state", name, field)
}
