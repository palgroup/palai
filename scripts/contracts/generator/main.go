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
	var goName, otherName strings.Builder
	for _, part := range strings.Split(def, "_") {
		if part == "id" {
			goName.WriteString("ID")
			otherName.WriteString("Id")
			continue
		}
		goName.WriteString(titleCase(part))
		otherName.WriteString(titleCase(part))
	}
	return identifier{
		Def:     def,
		Pattern: pattern,
		GoName:  goName.String(),
		GoVar:   lowerFirst(goName.String()) + "Pattern",
		TSName:  otherName.String(),
		TSVar:   lowerFirst(otherName.String()) + "Pattern",
		PyName:  otherName.String(),
		PyConst: strings.ToUpper(def) + "_PATTERN",
	}
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
