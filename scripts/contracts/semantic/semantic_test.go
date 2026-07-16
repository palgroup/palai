package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// canonicalFixture is a miniature OpenAPI document carrying one of each semantic
// construct the projection must preserve: const, enum, a nullable type union,
// required, and pattern.
func canonicalFixture() map[string]any {
	return map[string]any{
		"openapi": canonicalVersion,
		"info":    map[string]any{"title": "fixture", "version": "0"},
		"components": map[string]any{
			"schemas": map[string]any{
				"S": map[string]any{
					"type":     "object",
					"required": []any{"id", "kind"},
					"properties": map[string]any{
						"id":    map[string]any{"type": "string", "pattern": "^x_[a-z]+$"},
						"kind":  map[string]any{"const": "widget"},
						"state": map[string]any{"enum": []any{"open", "closed"}},
						"note":  map[string]any{"type": []any{"string", "null"}},
					},
				},
			},
		},
	}
}

func fixtureProperties(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	components := document["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	schema := schemas["S"].(map[string]any)
	return schema["properties"].(map[string]any)
}

func TestProjectDocumentBumpsOnlyTheVersion(t *testing.T) {
	canonical := canonicalFixture()
	data, err := projectDocument(canonical)
	if err != nil {
		t.Fatalf("projectDocument() error = %v", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatal("projection is not newline-terminated")
	}
	var projected map[string]any
	if err := json.Unmarshal(data, &projected); err != nil {
		t.Fatalf("decode projection error = %v", err)
	}
	if projected["openapi"] != projectionVersion {
		t.Fatalf("projected openapi = %v, want %s", projected["openapi"], projectionVersion)
	}
	// projectDocument must not mutate the canonical map it was handed.
	if canonical["openapi"] != canonicalVersion {
		t.Fatalf("canonical openapi mutated to %v", canonical["openapi"])
	}
	if !reflect.DeepEqual(withoutVersion(canonical), withoutVersion(projected)) {
		t.Fatal("projection changed the document body")
	}
}

func TestProjectDocumentIsDeterministic(t *testing.T) {
	first, err := projectDocument(canonicalFixture())
	if err != nil {
		t.Fatalf("projectDocument() error = %v", err)
	}
	second, err := projectDocument(canonicalFixture())
	if err != nil {
		t.Fatalf("projectDocument() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("projection is not deterministic")
	}
}

func TestProjectDocumentRejectsWrongVersion(t *testing.T) {
	canonical := canonicalFixture()
	canonical["openapi"] = "3.0.3"
	if _, err := projectDocument(canonical); err == nil {
		t.Fatal("projectDocument accepted a non-3.2 canonical document")
	}
}

func TestDiffAcceptsPureVersionBump(t *testing.T) {
	canonical := canonicalFixture()
	projection := canonicalFixture()
	projection["openapi"] = projectionVersion
	if err := diffDocuments(canonical, projection); err != nil {
		t.Fatalf("diffDocuments() error = %v, want nil", err)
	}
}

func TestDiffRejectsSemanticDrift(t *testing.T) {
	cases := map[string]func(properties map[string]any){
		"drops required": func(_ map[string]any) {},
		"changes pattern": func(properties map[string]any) {
			properties["id"].(map[string]any)["pattern"] = "^y_[a-z]+$"
		},
		"changes const": func(properties map[string]any) {
			properties["kind"].(map[string]any)["const"] = "gadget"
		},
		"narrows enum": func(properties map[string]any) {
			properties["state"].(map[string]any)["enum"] = []any{"open"}
		},
		"drops nullable": func(properties map[string]any) {
			properties["note"].(map[string]any)["type"] = "string"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			canonical := canonicalFixture()
			projection := canonicalFixture()
			projection["openapi"] = projectionVersion
			if name == "drops required" {
				schema := projection["components"].(map[string]any)["schemas"].(map[string]any)["S"].(map[string]any)
				schema["required"] = []any{"id"}
			} else {
				mutate(fixtureProperties(t, projection))
			}
			if err := diffDocuments(canonical, projection); err == nil {
				t.Fatalf("diffDocuments accepted drift: %s", name)
			}
		})
	}
}
