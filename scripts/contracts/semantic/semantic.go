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
	"sort"
	"strings"
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
// Because the projection only bumps the version, diffDocuments can never catch a
// canonical construct that has no 3.1.2 form; oapi32OnlyConstructs is the guard
// that does, failing loudly before an invalid-for-3.1.2 document is emitted.
//
// ponytail: oapi32OnlyConstructs is a fixed tripwire list (QUERY method,
// itemSchema); the upgrade is a full OpenAPI 3.1.2 validity check if the
// canonical starts using 3.2 features beyond that list.
func projectDocument(canonical map[string]any) ([]byte, error) {
	if canonical["openapi"] != canonicalVersion {
		return nil, fmt.Errorf("canonical openapi = %v, want %s", canonical["openapi"], canonicalVersion)
	}
	if blockers := oapi32OnlyConstructs(canonical); len(blockers) > 0 {
		return nil, fmt.Errorf("OpenAPI 3.2-only construct not representable in %s (ADR-0002 revisit trigger): %s",
			projectionVersion, strings.Join(blockers, "; "))
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

// oapi32OnlyConstructs reports OpenAPI 3.2 constructs that have no OpenAPI 3.1.2
// form, so the mechanical version bump cannot lower them. It is a deliberately
// small tripwire, not a validator: it fires so a canonical edit that reaches
// beyond 3.1.2 gets a human decision against ADR-0002's revisit trigger.
func oapi32OnlyConstructs(document map[string]any) []string {
	var found []string
	// The HTTP QUERY method is a 3.2 path-item operation absent from 3.1.2.
	if paths, ok := document["paths"].(map[string]any); ok {
		for path, raw := range paths {
			if item, ok := raw.(map[string]any); ok {
				if _, has := item["query"]; has {
					found = append(found, "paths."+path+".query (HTTP QUERY method)")
				}
			}
		}
	}
	// itemSchema is a 3.2 media-type field for sequential media, absent from 3.1.2.
	scanItemSchema(document, "$", &found)
	sort.Strings(found)
	return found
}

func scanItemSchema(node any, path string, found *[]string) {
	switch value := node.(type) {
	case map[string]any:
		if media, ok := value["content"].(map[string]any); ok {
			for mediaType, raw := range media {
				if body, ok := raw.(map[string]any); ok {
					if _, has := body["itemSchema"]; has {
						*found = append(*found, path+".content."+mediaType+".itemSchema (sequential media itemSchema)")
					}
				}
			}
		}
		for key, child := range value {
			scanItemSchema(child, path+"."+key, found)
		}
	case []any:
		for index, child := range value {
			scanItemSchema(child, fmt.Sprintf("%s[%d]", path, index), found)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
