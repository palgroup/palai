package contracts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	canonicalOpenAPIPath  = "protocols/openapi/openapi-3.2.yaml"
	projectionOpenAPIPath = "protocols/generated/openapi-3.1.2.yaml"
)

// httpMethods is the OpenAPI-defined set of operation keys under a path item.
var httpMethods = []string{"get", "put", "post", "delete", "patch", "options", "head", "trace"}

func readDoc(t *testing.T, rel string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode %s: %v", rel, err)
	}
	return doc
}

// resolveRef follows a single internal "#/..." reference; a node without $ref is
// returned unchanged so callers can treat inline and referenced objects alike.
func resolveRef(t *testing.T, doc, node map[string]any) map[string]any {
	t.Helper()
	ref, ok := node["$ref"].(string)
	if !ok {
		return node
	}
	if !strings.HasPrefix(ref, "#/") {
		t.Fatalf("expected internal reference, got %q", ref)
	}
	var cursor any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		object, ok := cursor.(map[string]any)
		if !ok {
			t.Fatalf("reference %q: %q is not an object", ref, part)
		}
		if cursor, ok = object[part]; !ok {
			t.Fatalf("reference %q: missing %q", ref, part)
		}
	}
	resolved, ok := cursor.(map[string]any)
	if !ok {
		t.Fatalf("reference %q does not resolve to an object", ref)
	}
	return resolved
}

func TestProjectionExistsAndIsRegenerated(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, projectionOpenAPIPath)
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("projection is not committed: %v", err)
	}
	// Regenerating must reproduce the committed bytes exactly; a mismatch means the
	// projection is stale or non-deterministic.
	if err := runMake(t, "generate"); err != nil {
		t.Fatalf("make generate: %v", err)
	}
	regenerated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(committed, regenerated) {
		t.Fatal("committed projection is stale; run make generate")
	}
}

func TestProjectionPreservesCanonicalSemantics(t *testing.T) {
	canonical := readDoc(t, canonicalOpenAPIPath)
	projection := readDoc(t, projectionOpenAPIPath)

	// The canonical constructs we must not drop have to actually be reachable, or
	// the equivalence check below would be vacuous.
	families := map[string]bool{}
	refs := map[string]bool{}
	collectRefs(canonical, refs)
	for ref := range refs {
		file := ref
		if hash := strings.IndexByte(file, '#'); hash >= 0 {
			file = file[:hash]
		}
		var schema any
		data, err := os.ReadFile(filepath.Join(repoRoot(t), "protocols/openapi", file))
		if err != nil {
			t.Fatalf("read referenced schema %s: %v", file, err)
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("decode referenced schema %s: %v", file, err)
		}
		scanFamilies(schema, families)
	}
	for _, family := range []string{"const", "enum", "nullable", "pattern", "required"} {
		if !families[family] {
			t.Fatalf("no referenced schema exercises %q; the preservation check is vacuous", family)
		}
	}

	// The projection is a pure version bump: the version changes and nothing else,
	// so every const/enum/nullable/required/pattern survives untouched.
	if canonical["openapi"] != "3.2.0" {
		t.Fatalf("canonical openapi = %v, want 3.2.0", canonical["openapi"])
	}
	if projection["openapi"] != "3.1.2" {
		t.Fatalf("projection openapi = %v, want 3.1.2", projection["openapi"])
	}
	delete(canonical, "openapi")
	delete(projection, "openapi")
	if !reflect.DeepEqual(canonical, projection) {
		t.Fatal("projection differs from canonical beyond the OpenAPI version")
	}
}

func TestOpenAPICoversLp0Surface(t *testing.T) {
	doc := readDoc(t, canonicalOpenAPIPath)
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("canonical document has no paths")
	}
	// LP-0 surface: exact path + method pairs the HTTP contract must expose.
	surface := []struct{ method, path string }{
		{"post", "/v1/responses"},
		{"get", "/v1/responses/{response_id}"},
		{"post", "/v1/responses/{response_id}/cancel"},
		{"get", "/v1/sessions/{session_id}/events"},
		{"get", "/v1/capabilities"},
	}
	for _, want := range surface {
		item, ok := paths[want.path].(map[string]any)
		if !ok {
			t.Fatalf("missing path %s", want.path)
		}
		if _, ok := item[want.method].(map[string]any); !ok {
			t.Fatalf("path %s missing %s operation", want.path, want.method)
		}
	}
}

func TestEveryOperationDeclaresProblemErrors(t *testing.T) {
	doc := readDoc(t, canonicalOpenAPIPath)
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("canonical document has no paths")
	}
	operations := 0
	for path, raw := range paths {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("path %s is not an object", path)
		}
		for _, method := range httpMethods {
			opRaw, ok := item[method]
			if !ok {
				continue
			}
			operations++
			op := opRaw.(map[string]any)
			responses, ok := op["responses"].(map[string]any)
			if !ok {
				t.Fatalf("%s %s has no responses", method, path)
			}
			def, ok := responses["default"].(map[string]any)
			if !ok {
				t.Fatalf("%s %s has no default response", method, path)
			}
			def = resolveRef(t, doc, def)
			content, ok := def["content"].(map[string]any)
			if !ok {
				t.Fatalf("%s %s default response carries no content", method, path)
			}
			media, ok := content["application/problem+json"].(map[string]any)
			if !ok {
				t.Fatalf("%s %s default response is not application/problem+json", method, path)
			}
			schema, ok := media["schema"].(map[string]any)
			if !ok {
				t.Fatalf("%s %s default problem media has no schema", method, path)
			}
			if ref, _ := schema["$ref"].(string); !strings.HasSuffix(ref, "problem.json") {
				t.Fatalf("%s %s default problem schema $ref = %q, want a problem.json reference", method, path, ref)
			}
		}
	}
	if operations == 0 {
		t.Fatal("canonical document declares no operations")
	}
}

// collectRefs gathers every "$ref" string that points at an external schema file.
func collectRefs(node any, out map[string]bool) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				if ref, ok := child.(string); ok && strings.Contains(ref, "schemas/") {
					out[ref] = true
				}
			}
			collectRefs(child, out)
		}
	case []any:
		for _, child := range value {
			collectRefs(child, out)
		}
	}
}

// scanFamilies records which canonical semantic constructs appear anywhere in a
// JSON Schema tree.
func scanFamilies(node any, seen map[string]bool) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			switch key {
			case "const":
				seen["const"] = true
			case "enum":
				seen["enum"] = true
			case "pattern":
				seen["pattern"] = true
			case "required":
				seen["required"] = true
			case "type":
				if types, ok := child.([]any); ok {
					for _, entry := range types {
						if entry == "null" {
							seen["nullable"] = true
						}
					}
				}
			}
			scanFamilies(child, seen)
		}
	case []any:
		for _, child := range value {
			scanFamilies(child, seen)
		}
	}
}
