package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"text/template"
)

//go:embed templates/*
var templateFiles embed.FS

type schemaIR struct {
	Title          string
	Required       []string
	AllowsUnknown  bool
	NoteOptional   bool
	NoteNullable   bool
	StatusOpen     bool
	SequenceFormat string
}

type outputTemplate struct {
	Template string
	Path     string
}

var outputs = []outputTemplate{
	{Template: "fixture.go.tmpl", Path: "go/fixture.go"},
	{Template: "fixture.py.tmpl", Path: "python/fixture.py"},
	{Template: "check.py.tmpl", Path: "python/check.py"},
	{Template: "fixture.ts.tmpl", Path: "typescript/fixture.ts"},
	{Template: "check.ts.tmpl", Path: "typescript/check.ts"},
	{Template: "tsconfig.json.tmpl", Path: "typescript/tsconfig.json"},
}

func main() {
	contractRoot := flag.String("root", "", "contracts source directory")
	outputRoot := flag.String("out", "", "generated output directory")
	flag.Parse()
	if *contractRoot == "" {
		repositoryRoot, err := commandOutput("git", "rev-parse", "--show-toplevel")
		if err != nil {
			fatal(err)
		}
		*contractRoot = filepath.Join(repositoryRoot, "spikes", "contracts")
	}
	if *outputRoot == "" {
		*outputRoot = filepath.Join(*contractRoot, "generated")
	}
	contractRootAbsolute, err := filepath.Abs(*contractRoot)
	if err != nil {
		fatal(err)
	}
	outputRootAbsolute, err := filepath.Abs(*outputRoot)
	if err != nil {
		fatal(err)
	}
	if contractRootAbsolute == outputRootAbsolute {
		fatal(errors.New("generated output cannot replace the contracts source directory"))
	}

	schema, err := readObject(filepath.Join(contractRootAbsolute, "schemas", "fixture.json"))
	if err != nil {
		fatal(err)
	}
	ir, err := buildIR(schema)
	if err != nil {
		fatal(err)
	}
	for _, output := range outputs {
		if err := renderTemplate(outputRootAbsolute, output, ir); err != nil {
			fatal(err)
		}
	}
	if err := generateOpenAPIProjection(contractRootAbsolute, outputRootAbsolute, schema); err != nil {
		fatal(err)
	}
}

func buildIR(schema map[string]any) (schemaIR, error) {
	title, _ := schema["title"].(string)
	rootType, _ := schema["type"].(string)
	allowsUnknown, _ := schema["additionalProperties"].(bool)
	properties, ok := schema["properties"].(map[string]any)
	if title != "Fixture" || rootType != "object" || !allowsUnknown || !ok {
		return schemaIR{}, errors.New("canonical schema must be the open Fixture object")
	}
	requiredValues, ok := schema["required"].([]any)
	if !ok {
		return schemaIR{}, errors.New("canonical schema required list is missing")
	}
	required := make([]string, 0, len(requiredValues))
	requiredSet := make(map[string]bool, len(requiredValues))
	for _, value := range requiredValues {
		name, ok := value.(string)
		if !ok {
			return schemaIR{}, errors.New("canonical schema required name is not a string")
		}
		required = append(required, name)
		requiredSet[name] = true
	}
	sort.Strings(required)
	for _, name := range []string{"id", "status", "metadata", "sequence", "created_at"} {
		if !requiredSet[name] {
			return schemaIR{}, fmt.Errorf("canonical schema field %s must be required", name)
		}
	}
	if requiredSet["note"] {
		return schemaIR{}, errors.New("canonical schema note must remain optional")
	}
	note, err := objectProperty(properties, "note")
	if err != nil {
		return schemaIR{}, err
	}
	noteTypes, ok := note["type"].([]any)
	if !ok || !containsString(noteTypes, "string") || !containsString(noteTypes, "null") {
		return schemaIR{}, errors.New("canonical schema note must be nullable string")
	}
	status, err := objectProperty(properties, "status")
	if err != nil {
		return schemaIR{}, err
	}
	if status["type"] != "string" {
		return schemaIR{}, errors.New("canonical schema status must be string-backed")
	}
	if _, closed := status["enum"]; closed {
		return schemaIR{}, errors.New("canonical schema status must remain open")
	}
	sequence, err := objectProperty(properties, "sequence")
	if err != nil {
		return schemaIR{}, err
	}
	if sequence["type"] != "integer" || sequence["format"] != "int64" {
		return schemaIR{}, errors.New("canonical schema sequence must be int64 integer")
	}
	metadata, err := objectProperty(properties, "metadata")
	if err != nil {
		return schemaIR{}, err
	}
	if metadata["type"] != "object" || metadata["additionalProperties"] != true {
		return schemaIR{}, errors.New("canonical schema metadata must preserve unknown fields")
	}
	return schemaIR{
		Title:          title,
		Required:       required,
		AllowsUnknown:  true,
		NoteOptional:   true,
		NoteNullable:   true,
		StatusOpen:     true,
		SequenceFormat: "int64",
	}, nil
}

func objectProperty(properties map[string]any, name string) (map[string]any, error) {
	value, ok := properties[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("canonical schema property %s is missing", name)
	}
	return value, nil
}

func containsString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func renderTemplate(root string, output outputTemplate, ir schemaIR) error {
	parsed, err := template.New(output.Template).Option("missingkey=error").ParseFS(templateFiles, "templates/"+output.Template)
	if err != nil {
		return fmt.Errorf("parse template %s: %w", output.Template, err)
	}
	var rendered bytes.Buffer
	if err := parsed.ExecuteTemplate(&rendered, output.Template, ir); err != nil {
		return fmt.Errorf("execute template %s: %w", output.Template, err)
	}
	return writeGenerated(filepath.Join(root, output.Path), rendered.Bytes())
}

func generateOpenAPIProjection(contractRoot, outputRoot string, canonicalSchema map[string]any) error {
	document, err := readObject(filepath.Join(contractRoot, "openapi-3.2.yaml"))
	if err != nil {
		return err
	}
	if document["openapi"] != "3.2.0" {
		return errors.New("canonical OpenAPI document must be version 3.2.0")
	}
	components, ok := document["components"].(map[string]any)
	if !ok {
		return errors.New("canonical OpenAPI components are missing")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		return errors.New("canonical OpenAPI schemas are missing")
	}
	fixture, ok := schemas["Fixture"].(map[string]any)
	if !ok {
		return errors.New("canonical OpenAPI Fixture schema is missing")
	}
	delete(canonicalSchema, "$schema")
	delete(canonicalSchema, "$id")
	if !reflect.DeepEqual(fixture, canonicalSchema) {
		return errors.New("canonical OpenAPI Fixture differs from canonical JSON Schema")
	}
	document["openapi"] = "3.1.2"
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal OpenAPI projection: %w", err)
	}
	return writeGenerated(filepath.Join(outputRoot, "openapi-3.1.2.yaml"), append(data, '\n'))
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

func commandOutput(name string, arguments ...string) (string, error) {
	command := exec.Command(name, arguments...)
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("run %s: %w", name, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
