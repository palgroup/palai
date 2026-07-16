// Command generator promotes canonical JSON Schema contracts into the
// per-language sources under packages/contracts and protocols/generated.
//
// Generation is deterministic: schema $defs are emitted in sorted order and Go
// output is run through go/format, so a second run over unchanged schemas
// produces a zero diff. Each canonical schema family is handled by its own
// generate* function; later contract tasks add siblings here.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

//go:embed templates/*
var templateFiles embed.FS

func main() {
	schemasRoot := flag.String("schemas", "", "canonical JSON Schema source directory")
	goOut := flag.String("go-out", "", "Go output directory (packages/contracts)")
	tsOut := flag.String("ts-out", "", "TypeScript output directory")
	pyOut := flag.String("py-out", "", "Python output directory")
	flag.Parse()
	for name, value := range map[string]string{
		"-schemas": *schemasRoot,
		"-go-out":  *goOut,
		"-ts-out":  *tsOut,
		"-py-out":  *pyOut,
	} {
		if value == "" {
			fatal(fmt.Errorf("%s is required", name))
		}
	}
	if err := generateIdentifiers(*schemasRoot, *goOut, *tsOut, *pyOut); err != nil {
		fatal(err)
	}
	if err := generateObjects(*schemasRoot, *goOut); err != nil {
		fatal(err)
	}
}

// identifier is one opaque-ID $def projected into each target language.
type identifier struct {
	Def     string // snake_case $def key, e.g. "organization_id"
	Pattern string // canonical URL-safe pattern
	GoName  string // "OrganizationID"
	GoVar   string // "organizationIDPattern"
	TSName  string // "OrganizationId"
	TSVar   string // "organizationIdPattern"
	PyName  string // "OrganizationId"
	PyConst string // "ORGANIZATION_ID_PATTERN"
}

type identifierSchema struct {
	Title       string
	Identifiers []identifier
}

func generateIdentifiers(schemasRoot, goOut, tsOut, pyOut string) error {
	schema, err := readObject(filepath.Join(schemasRoot, "common", "id.json"))
	if err != nil {
		return err
	}
	ir, err := buildIdentifierIR(schema)
	if err != nil {
		return err
	}
	goSource, err := execTemplate("ids.go.tmpl", ir)
	if err != nil {
		return err
	}
	formatted, err := format.Source(goSource)
	if err != nil {
		return fmt.Errorf("format generated go: %w", err)
	}
	if err := writeGenerated(filepath.Join(goOut, "ids.gen.go"), formatted); err != nil {
		return err
	}
	for _, out := range []struct {
		template string
		path     string
	}{
		{"ids.ts.tmpl", filepath.Join(tsOut, "ids.ts")},
		{"ids.py.tmpl", filepath.Join(pyOut, "ids.py")},
	} {
		source, err := execTemplate(out.template, ir)
		if err != nil {
			return err
		}
		if err := writeGenerated(out.path, source); err != nil {
			return err
		}
	}
	return nil
}

func buildIdentifierIR(schema map[string]any) (identifierSchema, error) {
	title, _ := schema["title"].(string)
	if title == "" {
		return identifierSchema{}, errors.New("identifier schema title is missing")
	}
	defs, ok := schema["$defs"].(map[string]any)
	if !ok || len(defs) == 0 {
		return identifierSchema{}, errors.New("identifier schema $defs is missing")
	}
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	sort.Strings(names)
	identifiers := make([]identifier, 0, len(names))
	for _, name := range names {
		def, ok := defs[name].(map[string]any)
		if !ok {
			return identifierSchema{}, fmt.Errorf("$defs %s is not an object", name)
		}
		if def["type"] != "string" {
			return identifierSchema{}, fmt.Errorf("$defs %s must be a string type", name)
		}
		pattern, ok := def["pattern"].(string)
		if !ok || pattern == "" {
			return identifierSchema{}, fmt.Errorf("$defs %s must declare a pattern", name)
		}
		identifiers = append(identifiers, newIdentifier(name, pattern))
	}
	return identifierSchema{Title: title, Identifiers: identifiers}, nil
}

// newIdentifier derives the per-language names from a snake_case $def key. The
// "id" segment becomes "ID" in Go (stdlib convention) and "Id" elsewhere.
func newIdentifier(def, pattern string) identifier {
	goName := goExportedName(def)
	var otherName strings.Builder
	for _, part := range strings.Split(def, "_") {
		otherName.WriteString(titleCase(part))
	}
	return identifier{
		Def:     def,
		Pattern: pattern,
		GoName:  goName,
		GoVar:   lowerFirst(goName) + "Pattern",
		TSName:  otherName.String(),
		TSVar:   lowerFirst(otherName.String()) + "Pattern",
		PyName:  otherName.String(),
		PyConst: strings.ToUpper(def) + "_PATTERN",
	}
}

// goExportedName turns a snake_case key into an exported Go identifier. The "id"
// segment becomes "ID" to follow the stdlib initialism convention.
func goExportedName(snake string) string {
	var b strings.Builder
	for _, part := range strings.Split(snake, "_") {
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		b.WriteString(titleCase(part))
	}
	return b.String()
}

func titleCase(segment string) string {
	if segment == "" {
		return ""
	}
	return strings.ToUpper(segment[:1]) + segment[1:]
}

func lowerFirst(segment string) string {
	if segment == "" {
		return ""
	}
	return strings.ToLower(segment[:1]) + segment[1:]
}

// objectSchema is one canonical object schema projected into Go structs: the
// root object (when the schema declares one) plus every object-typed $def.
type objectSchema struct {
	Title   string
	Structs []goStruct
}

type goStruct struct {
	Name   string
	Fields []goField
}

type goField struct {
	GoName  string // exported Go field name, e.g. "RequestID"
	GoType  string // Go type, e.g. "string", "int", "RequestID", "[]any"
	JSONTag string // json tag body, e.g. "request_id" or "detail,omitempty"
}

// generateObjects promotes every canonical object schema (all but the
// identifier seed) into a package-contracts Go source. Schemas are globbed
// across their directories in sorted order and structs/fields are sorted, so a
// second run is a zero diff. A schema whose root allOf is a set of if/then
// branches is an open union (ADR-0002): it becomes a raw-preserving map wrapper
// that keeps unknown fields and unknown discriminator values. A root allOf that
// only carries a cross-field constraint (e.g. a mutual-exclusion `not`) leaves
// the schema an ordinary fixed-field struct.
func generateObjects(schemasRoot, goOut string) error {
	files, err := filepath.Glob(filepath.Join(schemasRoot, "*", "*.json"))
	if err != nil {
		return err
	}
	// The engine and runner JSONL frame schemas live under sibling protocol roots
	// (protocols/engine, protocols/runner), outside the canonical schemas tree.
	protocolsRoot := filepath.Dir(schemasRoot)
	for _, dir := range []string{"engine", "runner"} {
		more, err := filepath.Glob(filepath.Join(protocolsRoot, dir, "*.json"))
		if err != nil {
			return err
		}
		files = append(files, more...)
	}
	sort.Strings(files)
	for _, file := range files {
		base := filepath.Base(file)
		if base == "id.json" {
			continue // identifier seed is handled by generateIdentifiers
		}
		if base == "event-types.json" {
			continue // event registry is a data file (a name list), not a typed schema
		}
		schema, err := readObject(file)
		if err != nil {
			return err
		}
		var (
			ir       any
			tmplName string
		)
		if isOpenUnion(schema) {
			tmplName = "union.go.tmpl"
			ir, err = buildUnionIR(schema)
		} else {
			tmplName = "object.go.tmpl"
			ir, err = buildObjectIR(schema, filepath.Dir(file))
		}
		if err != nil {
			return fmt.Errorf("%s: %w", base, err)
		}
		source, err := execTemplate(tmplName, ir)
		if err != nil {
			return err
		}
		formatted, err := format.Source(source)
		if err != nil {
			return fmt.Errorf("format generated go for %s: %w", base, err)
		}
		out := filepath.Join(goOut, strings.TrimSuffix(base, ".json")+".gen.go")
		if err := writeGenerated(out, formatted); err != nil {
			return err
		}
	}
	return nil
}

// unionSchema is one open-union schema projected into a raw-preserving Go type:
// a map wrapper that keeps unknown fields and unknown discriminator values on
// round-trip, with a typed accessor for the discriminator field.
type unionSchema struct {
	Title            string // exported Go type, e.g. "ContentItem"
	Discriminator    string // JSON discriminator field, e.g. "type"
	DiscriminatorTag string // exported accessor name, e.g. "Type"
}

// isOpenUnion reports whether a root allOf marks a discriminated open union
// (if/then branches keyed on a discriminator, e.g. content.json) rather than an
// object schema that merely carries a cross-field constraint (e.g.
// response-create.json's mutual-exclusion `not`).
func isOpenUnion(schema map[string]any) bool {
	branches, ok := schema["allOf"].([]any)
	if !ok || len(branches) == 0 {
		return false
	}
	// A pure discriminated union fixes only the discriminator (content.json). A
	// schema that also declares a full envelope (engine/runner frames) is a fixed
	// struct whose if/then branches merely refine each type's payload.
	if props, _ := schema["properties"].(map[string]any); len(props) != 1 {
		return false
	}
	for _, raw := range branches {
		entry, _ := raw.(map[string]any)
		if _, isBranch := entry["if"]; !isBranch {
			return false
		}
	}
	return true
}

func buildUnionIR(schema map[string]any) (unionSchema, error) {
	title, _ := schema["title"].(string)
	if title == "" {
		return unionSchema{}, errors.New("union schema title is missing")
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) != 1 {
		return unionSchema{}, fmt.Errorf("union schema must declare exactly one discriminator property, got %d", len(props))
	}
	discriminator := sortedKeys(props)[0]
	return unionSchema{
		Title:            title,
		Discriminator:    discriminator,
		DiscriminatorTag: goExportedName(discriminator),
	}, nil
}

func buildObjectIR(schema map[string]any, schemaDir string) (objectSchema, error) {
	title, _ := schema["title"].(string)
	if title == "" {
		return objectSchema{}, errors.New("schema title is missing")
	}
	defs, _ := schema["$defs"].(map[string]any)
	var structs []goStruct
	if schema["type"] == "object" {
		root, err := buildStruct(title, schema, defs, schemaDir)
		if err != nil {
			return objectSchema{}, err
		}
		structs = append(structs, root)
	}
	for _, name := range sortedKeys(defs) {
		def, ok := defs[name].(map[string]any)
		if !ok || def["type"] != "object" {
			continue // string, enum, and other $defs are only referenced, not structs
		}
		s, err := buildStruct(goExportedName(name), def, defs, schemaDir)
		if err != nil {
			return objectSchema{}, err
		}
		structs = append(structs, s)
	}
	if len(structs) == 0 {
		return objectSchema{}, errors.New("schema defines no object types")
	}
	sort.Slice(structs, func(i, j int) bool { return structs[i].Name < structs[j].Name })
	return objectSchema{Title: title, Structs: structs}, nil
}

func buildStruct(name string, obj, defs map[string]any, schemaDir string) (goStruct, error) {
	props, _ := obj["properties"].(map[string]any)
	required := stringSet(obj["required"])
	fields := make([]goField, 0, len(props))
	for _, prop := range sortedKeys(props) {
		schema, ok := props[prop].(map[string]any)
		if !ok {
			return goStruct{}, fmt.Errorf("property %s is not an object", prop)
		}
		typ, err := goFieldType(schema, defs, schemaDir)
		if err != nil {
			return goStruct{}, fmt.Errorf("property %s: %w", prop, err)
		}
		tag := prop
		if !required[prop] {
			tag += ",omitempty"
		}
		fields = append(fields, goField{GoName: goExportedName(prop), GoType: typ, JSONTag: tag})
	}
	return goStruct{Name: name, Fields: fields}, nil
}

// goFieldType maps a property schema to its Go type. $ref resolves to a
// generated identifier type or a local $def; a nullable type becomes a pointer.
func goFieldType(schema, defs map[string]any, schemaDir string) (string, error) {
	if ref, ok := schema["$ref"].(string); ok {
		return refType(ref, defs, schemaDir)
	}
	kind, nullable, err := jsonType(schema)
	if err != nil {
		return "", err
	}
	var base string
	switch kind {
	case "string":
		base = "string"
	case "integer":
		base = "int"
	case "number":
		base = "float64"
	case "boolean":
		base = "bool"
	case "object":
		base = objectFieldType(schema)
	case "array":
		item, err := itemType(schema, defs, schemaDir)
		if err != nil {
			return "", err
		}
		base = "[]" + item
	default:
		base = "any"
	}
	if nullable {
		return "*" + base, nil
	}
	return base, nil
}

// jsonType reads a JSON Schema "type", which may be a string or a ["T","null"]
// array. It returns the concrete type and whether null is permitted.
func jsonType(schema map[string]any) (kind string, nullable bool, err error) {
	switch t := schema["type"].(type) {
	case string:
		return t, false, nil
	case []any:
		for _, v := range t {
			s, _ := v.(string)
			if s == "null" {
				nullable = true
				continue
			}
			if kind == "" {
				kind = s
			}
		}
		if kind == "" {
			return "", false, errors.New("type array declares no concrete type")
		}
		return kind, nullable, nil
	case nil:
		return "", false, nil // untyped schema maps to any
	default:
		return "", false, fmt.Errorf("unsupported type declaration %T", t)
	}
}

func objectFieldType(schema map[string]any) string {
	if ap, ok := schema["additionalProperties"].(map[string]any); ok && ap["type"] == "string" {
		return "map[string]string"
	}
	return "map[string]any"
}

func itemType(schema, defs map[string]any, schemaDir string) (string, error) {
	items, ok := schema["items"].(map[string]any)
	if !ok {
		return "any", nil // untyped array elements
	}
	return goFieldType(items, defs, schemaDir)
}

// refType maps a JSON Schema $ref to its Go type. It resolves local $defs, the
// canonical identifier defs in common/id.json (referenced from any directory
// depth), and a sibling schema root ref to that schema's generated title type.
func refType(ref string, defs map[string]any, schemaDir string) (string, error) {
	if name, ok := strings.CutPrefix(ref, "#/$defs/"); ok {
		def, ok := defs[name].(map[string]any)
		if !ok {
			return "", fmt.Errorf("unresolved local $ref %s", ref)
		}
		return goFieldType(def, defs, schemaDir)
	}
	path, fragment, _ := strings.Cut(ref, "#")
	base := filepath.Base(path)
	if base == "id.json" {
		key, ok := strings.CutPrefix(fragment, "/$defs/")
		if !ok {
			return "", fmt.Errorf("unsupported id.json $ref %s", ref)
		}
		return goExportedName(key), nil
	}
	if fragment == "" && strings.HasSuffix(base, ".json") {
		return siblingTitle(filepath.Join(schemaDir, base))
	}
	return "", fmt.Errorf("unsupported $ref %s", ref)
}

// siblingTitle reads a sibling schema's title so a root $ref resolves to the
// Go type generated from that schema.
func siblingTitle(path string) (string, error) {
	schema, err := readObject(path)
	if err != nil {
		return "", err
	}
	title, _ := schema["title"].(string)
	if title == "" {
		return "", fmt.Errorf("sibling schema %s has no title", filepath.Base(path))
	}
	return title, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stringSet(raw any) map[string]bool {
	list, _ := raw.([]any)
	set := make(map[string]bool, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			set[s] = true
		}
	}
	return set
}

func execTemplate(name string, data any) ([]byte, error) {
	parsed, err := template.New(name).Option("missingkey=error").ParseFS(templateFiles, "templates/"+name)
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}
	var rendered bytes.Buffer
	if err := parsed.ExecuteTemplate(&rendered, name, data); err != nil {
		return nil, fmt.Errorf("execute template %s: %w", name, err)
	}
	return rendered.Bytes(), nil
}

func readObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return object, nil
}

func writeGenerated(path string, data []byte) error {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
