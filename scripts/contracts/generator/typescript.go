package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// tsType is one generated TypeScript transport type: a fixed-field interface, or —
// for an open union — an interface with an index signature so unknown fields and
// unknown discriminator values survive a round-trip (ADR-0002, spec API-009).
type tsType struct {
	Name   string
	Doc    string
	Open   bool
	Fields []tsField
}

type tsField struct {
	Name     string
	Type     string
	Optional bool
}

type tsSchema struct {
	IDNames []string // referenced identifier type aliases, inlined so types.ts is self-contained
	Types   []tsType
}

// generateSDKTypes projects the canonical object schemas into the single, self-contained
// TypeScript transport surface the SDK binds to (sdks/typescript/src/generated/types.ts).
// Only the public canonical schemas under protocols/schemas are emitted — never the
// engine/runner frame protocols — so the SDK carries the HTTP contract types and nothing
// internal. Identifier $refs are inlined as string aliases, so the file needs no
// cross-package import. Emission is deterministic (schemas globbed and types sorted), so a
// second run over unchanged schemas is a zero diff, under the same drift discipline as the
// Go/Python output.
func generateSDKTypes(schemasRoot, sdkTSOut string) error {
	files, err := filepath.Glob(filepath.Join(schemasRoot, "*", "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	idNames := map[string]bool{}
	var types []tsType
	for _, file := range files {
		base := filepath.Base(file)
		if base == "id.json" || base == "event-types.json" {
			continue // identifier seed and the event-name registry are not typed objects
		}
		schema, err := readObject(file)
		if err != nil {
			return err
		}
		more, err := buildTSTypes(schema, filepath.Dir(file), idNames)
		if err != nil {
			return fmt.Errorf("%s: %w", base, err)
		}
		types = append(types, more...)
	}
	sort.Slice(types, func(i, j int) bool { return types[i].Name < types[j].Name })
	ids := make([]string, 0, len(idNames))
	for name := range idNames {
		ids = append(ids, name)
	}
	sort.Strings(ids)
	source, err := execTemplate("types.ts.tmpl", tsSchema{IDNames: ids, Types: types})
	if err != nil {
		return err
	}
	return writeGenerated(filepath.Join(sdkTSOut, "types.ts"), source)
}

// buildTSTypes projects one canonical schema into its TypeScript types: an open union
// becomes an index-signature interface; an object schema becomes the root interface (when
// it declares one) plus an interface per object-typed $def.
func buildTSTypes(schema map[string]any, schemaDir string, idNames map[string]bool) ([]tsType, error) {
	title, _ := schema["title"].(string)
	if title == "" {
		return nil, errors.New("schema title is missing")
	}
	if isOpenUnion(schema) {
		props, _ := schema["properties"].(map[string]any)
		disc := sortedKeys(props)[0]
		return []tsType{{
			Name:   title,
			Doc:    "open union: unknown fields and unknown " + disc + " values survive a round-trip (ADR-0002, spec API-009).",
			Open:   true,
			Fields: []tsField{{Name: disc, Type: "string"}},
		}}, nil
	}
	defs, _ := schema["$defs"].(map[string]any)
	var out []tsType
	if schema["type"] == "object" {
		t, err := buildTSStruct(title, schema, defs, schemaDir, idNames)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	for _, name := range sortedKeys(defs) {
		def, ok := defs[name].(map[string]any)
		if !ok || def["type"] != "object" {
			continue // string, enum, and other $defs are only referenced, not emitted
		}
		t, err := buildTSStruct(goExportedName(name), def, defs, schemaDir, idNames)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, errors.New("schema defines no object types")
	}
	return out, nil
}

func buildTSStruct(name string, obj, defs map[string]any, schemaDir string, idNames map[string]bool) (tsType, error) {
	props, _ := obj["properties"].(map[string]any)
	required := stringSet(obj["required"])
	fields := make([]tsField, 0, len(props))
	for _, prop := range sortedKeys(props) {
		schema, ok := props[prop].(map[string]any)
		if !ok {
			return tsType{}, fmt.Errorf("property %s is not an object", prop)
		}
		typ, err := tsFieldType(schema, defs, schemaDir, idNames)
		if err != nil {
			return tsType{}, fmt.Errorf("property %s: %w", prop, err)
		}
		fields = append(fields, tsField{Name: prop, Type: typ, Optional: !required[prop]})
	}
	return tsType{Name: name, Fields: fields}, nil
}

// tsFieldType maps a property schema to its TypeScript type, mirroring goFieldType. A
// oneOf becomes a TypeScript union (so nullable refs like error keep their Problem type
// rather than collapsing to unknown as the Go view does).
func tsFieldType(schema, defs map[string]any, schemaDir string, idNames map[string]bool) (string, error) {
	if ref, ok := schema["$ref"].(string); ok {
		return tsRefType(ref, defs, schemaDir, idNames)
	}
	if branches, ok := schema["oneOf"].([]any); ok {
		return tsOneOf(branches, defs, schemaDir, idNames)
	}
	kind, nullable, err := jsonType(schema)
	if err != nil {
		return "", err
	}
	var base string
	switch kind {
	case "string":
		base = "string"
	case "integer", "number":
		base = "number"
	case "boolean":
		base = "boolean"
	case "null":
		base = "null"
	case "object":
		base = tsObjectFieldType(schema)
	case "array":
		item, err := tsItemType(schema, defs, schemaDir, idNames)
		if err != nil {
			return "", err
		}
		base = item + "[]"
	default:
		base = "unknown"
	}
	if nullable && base != "null" {
		return base + " | null", nil
	}
	return base, nil
}

// tsOneOf renders a oneOf as a deduplicated TypeScript union; a {"type":"null"} branch
// becomes the null literal.
func tsOneOf(branches []any, defs map[string]any, schemaDir string, idNames map[string]bool) (string, error) {
	parts := make([]string, 0, len(branches))
	seen := map[string]bool{}
	for _, raw := range branches {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, err := tsFieldType(b, defs, schemaDir, idNames)
		if err != nil {
			return "", err
		}
		if !seen[t] {
			seen[t] = true
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return "unknown", nil
	}
	return strings.Join(parts, " | "), nil
}

func tsObjectFieldType(schema map[string]any) string {
	if ap, ok := schema["additionalProperties"].(map[string]any); ok && ap["type"] == "string" {
		return "Record<string, string>"
	}
	return "Record<string, unknown>"
}

func tsItemType(schema, defs map[string]any, schemaDir string, idNames map[string]bool) (string, error) {
	items, ok := schema["items"].(map[string]any)
	if !ok {
		return "unknown", nil // untyped array elements
	}
	return tsFieldType(items, defs, schemaDir, idNames)
}

// tsRefType maps a $ref to its TypeScript type: a local $def resolves inline, an
// id.json identifier resolves to its inlined string alias, and a sibling schema root
// $ref resolves to that schema's generated title type.
func tsRefType(ref string, defs map[string]any, schemaDir string, idNames map[string]bool) (string, error) {
	if name, ok := strings.CutPrefix(ref, "#/$defs/"); ok {
		def, ok := defs[name].(map[string]any)
		if !ok {
			return "", fmt.Errorf("unresolved local $ref %s", ref)
		}
		return tsFieldType(def, defs, schemaDir, idNames)
	}
	path, fragment, _ := strings.Cut(ref, "#")
	base := filepath.Base(path)
	if base == "id.json" {
		key, ok := strings.CutPrefix(fragment, "/$defs/")
		if !ok {
			return "", fmt.Errorf("unsupported id.json $ref %s", ref)
		}
		name := tsIdentifierName(key)
		idNames[name] = true
		return name, nil
	}
	if fragment == "" && strings.HasSuffix(base, ".json") {
		// Resolve the full relative path (e.g. ../common/problem.json), not just the
		// base: unlike the Go view — which widens every oneOf to any and so never
		// resolves a cross-directory sibling — the TS union keeps the referenced type.
		return siblingTitle(filepath.Join(schemaDir, path))
	}
	return "", fmt.Errorf("unsupported $ref %s", ref)
}

// tsIdentifierName mirrors newIdentifier's TSName: snake_case to PascalCase, so a
// referenced id.json $def resolves to the same alias ids.ts declares.
func tsIdentifierName(snake string) string {
	var b strings.Builder
	for _, part := range strings.Split(snake, "_") {
		b.WriteString(titleCase(part))
	}
	return b.String()
}
