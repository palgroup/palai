// Command semantic derives the OpenAPI 3.1.2 compatibility projection from the
// canonical 3.2 document and verifies the two are semantically equivalent.
//
// The canonical document is never downgraded (ADR-0002): the projection exists
// only so generators that cannot read OpenAPI 3.2 have a 3.1.2 artifact to
// consume. The `project` mode writes that artifact; the `diff` mode proves the
// committed artifact is the canonical document with nothing but the version
// changed.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

const (
	canonicalVersion  = "3.2.0"
	projectionVersion = "3.1.2"
)

func main() {
	mode := flag.String("mode", "", "project | diff")
	canonical := flag.String("canonical", "", "canonical OpenAPI 3.2 document")
	out := flag.String("out", "", "projection output path (project mode)")
	projection := flag.String("projection", "", "committed projection path (diff mode)")
	flag.Parse()

	switch *mode {
	case "project":
		if err := writeProjection(*canonical, *out); err != nil {
			fatal(err)
		}
	case "diff":
		if err := checkProjection(*canonical, *projection); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown -mode %q (want project or diff)", *mode))
	}
}

func loadDocument(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return document, nil
}

// projectDocument returns the mechanical OpenAPI 3.1.2 projection of a canonical
// 3.2 document: it bumps only the version, so the compatibility artifact stays
// byte-verifiable against its source. Deterministic key ordering comes from
// json.MarshalIndent.
//
// ponytail: the version bump is the entire projection today; if the projection
// ever rewrites schemas, replace diffDocuments' equality with a schema-aware
// comparison.
func projectDocument(canonical map[string]any) ([]byte, error) {
	if canonical["openapi"] != canonicalVersion {
		return nil, fmt.Errorf("canonical openapi = %v, want %s", canonical["openapi"], canonicalVersion)
	}
	projected := make(map[string]any, len(canonical))
	for key, value := range canonical {
		projected[key] = value
	}
	projected["openapi"] = projectionVersion
	data, err := json.MarshalIndent(projected, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal projection: %w", err)
	}
	return append(data, '\n'), nil
}

func writeProjection(canonicalPath, outPath string) error {
	if canonicalPath == "" || outPath == "" {
		return errors.New("project mode requires -canonical and -out")
	}
	canonical, err := loadDocument(canonicalPath)
	if err != nil {
		return err
	}
	data, err := projectDocument(canonical)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(outPath), err)
	}
	return nil
}

// diffDocuments confirms the projection is the canonical document with only the
// OpenAPI version changed. Any other difference is semantic drift and preserves
// nothing we can trust, so it is rejected.
func diffDocuments(canonical, projection map[string]any) error {
	if canonical["openapi"] != canonicalVersion {
		return fmt.Errorf("canonical openapi = %v, want %s", canonical["openapi"], canonicalVersion)
	}
	if projection["openapi"] != projectionVersion {
		return fmt.Errorf("projection openapi = %v, want %s", projection["openapi"], projectionVersion)
	}
	if !reflect.DeepEqual(withoutVersion(canonical), withoutVersion(projection)) {
		return errors.New("projection differs from canonical beyond the OpenAPI version")
	}
	return nil
}

func withoutVersion(document map[string]any) map[string]any {
	body := make(map[string]any, len(document))
	for key, value := range document {
		if key == "openapi" {
			continue
		}
		body[key] = value
	}
	return body
}

func checkProjection(canonicalPath, projectionPath string) error {
	if canonicalPath == "" || projectionPath == "" {
		return errors.New("diff mode requires -canonical and -projection")
	}
	canonical, err := loadDocument(canonicalPath)
	if err != nil {
		return err
	}
	projection, err := loadDocument(projectionPath)
	if err != nil {
		return err
	}
	return diffDocuments(canonical, projection)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
