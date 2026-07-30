package identity

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// LAYER 2 OF E25's THREE-LAYER "no secret value comes back" PROOF (plan §2): the PROJECTION pin.
//
// Layer 1 is storage/ciphertext_sweep_test.go (no query reads the column). Layer 3 is the byte scan in
// the console journey (no response body carries the sentinel). This layer is the one in between, and it
// catches the case the other two cannot: a struct field added to a view that an EXISTING query could
// populate — `Value string` beside `Version int`, filled from anywhere, wired to a route later.
//
// It pins the field set by EXACT SET COMPARISON rather than by checking for the absence of a field called
// "value". The difference matters and this tree has paid for it more than once: a check for a forbidden
// NAME passes for `Plaintext`, `Secret`, `Cleartext`, `Bytes`, `Decrypted` and `V`. An exact set fails for
// all of them, including the names nobody thought of.
func TestSecretAndEnvironmentViewsCarryNoValueField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		zero   any
		fields []string
	}{
		{"secretRefView", secretRefView{}, []string{"name", "object", "updated_at", "version"}},
		{"environmentView", environmentView{}, []string{"created_at", "description", "id", "key_count", "keys", "name", "object"}},
		{"environmentKeyView", environmentKeyView{}, []string{"key", "object", "updated_at", "version"}},
	} {
		got := jsonFieldNames(reflect.TypeOf(tc.zero))
		slices.Sort(got)
		want := slices.Clone(tc.fields)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf(`%s JSON fields = %v, want %v

This is the projection layer of E25's no-value-comes-back proof. The query layer
(storage/ciphertext_sweep_test.go) stops a value from being READ; this stops one from being CARRIED. If a
field was added deliberately, change the list WITH the reason — and if the new field is a secret value,
that reason has to survive plan §2, which does not permit one.`, tc.name, got, want)
		}
	}
}

// TestTheViewFieldExtractorActuallyReadsJSONTags is the non-vacuity leg. A reflect-based set comparison
// rots in one quiet way: an extractor that silently SKIPPED a field would make every pin above pass
// while the struct grew. (An extractor returning nothing, or returning Go field names, fails loudly on
// its own.) So this asserts the extractor sees what the struct is known to have, and that the marshaller
// agrees with reflect — a custom MarshalJSON or an embedded struct could make them disagree.
func TestTheViewFieldExtractorActuallyReadsJSONTags(t *testing.T) {
	names := jsonFieldNames(reflect.TypeOf(environmentKeyView{}))
	if len(names) != 4 {
		t.Fatalf("the extractor found %d fields on environmentKeyView (%v); it is not reading the struct and the pins above are vacuous", len(names), names)
	}
	// A json TAG, not the Go field name. `UpdatedAt` would pass a name check and fail this one.
	if !slices.Contains(names, "updated_at") {
		t.Fatalf("the extractor returned %v — it is reading Go field names rather than JSON tags, so a renamed tag would slip past the pins above", names)
	}

	now := time.Now()
	raw, err := json.Marshal(environmentKeyView{Key: "K", Object: "environment_key", Version: 1, UpdatedAt: &now})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, name := range names {
		if !strings.Contains(string(raw), `"`+name+`"`) {
			t.Errorf("reflect reports a field %q that the marshaller does not emit: %s", name, raw)
		}
	}
}

// jsonFieldNames returns the JSON names a struct type marshals to: the tag name where one is set, the Go
// field name otherwise (encoding/json's own rule), with `-` tags skipped the way the marshaller skips
// them. Unexported fields are skipped because the marshaller never emits them.
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch tag {
		case "-":
			continue
		case "":
			names = append(names, f.Name)
		default:
			names = append(names, tag)
		}
	}
	return names
}
