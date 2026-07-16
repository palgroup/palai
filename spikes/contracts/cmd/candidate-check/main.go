package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type findingDocument struct {
	SchemaVersion int       `json:"schema_version"`
	Candidates    []finding `json:"candidates"`
}

type finding struct {
	Name             string `json:"name"`
	Version          string `json:"version"`
	InputDialect     string `json:"input_dialect"`
	ExpectedExit     int    `json:"expected_exit"`
	Status           string `json:"status"`
	EmittedSemantics string `json:"emitted_semantics"`
	RequiresWrapper  bool   `json:"requires_wrapper"`
	Reason           string `json:"reason"`
}

type summaryDocument struct {
	SchemaVersion int       `json:"schema_version"`
	Candidates    []summary `json:"candidates"`
}

type summary struct {
	finding
	ExitCode     int    `json:"exit_code"`
	OutputBytes  int    `json:"output_bytes"`
	OutputSHA256 string `json:"output_sha256"`
}

type candidateFiles struct {
	Output string
	Status string
	Stderr string
}

func main() {
	findingsPath := flag.String("findings", "", "candidate findings JSON")
	candidateDirectory := flag.String("candidate-dir", "", "raw candidate output directory")
	outputPath := flag.String("out", "", "validated summary JSON")
	flag.Parse()
	if *findingsPath == "" || *candidateDirectory == "" || *outputPath == "" {
		fatal(errors.New("-findings, -candidate-dir and -out are required"))
	}
	findings, err := readFindings(*findingsPath)
	if err != nil {
		fatal(err)
	}
	files := map[string]candidateFiles{
		"json-schema-to-typescript": {
			Output: "json-schema-to-typescript.ts",
			Status: "json-schema-to-typescript.status",
			Stderr: "json-schema-to-typescript.stderr",
		},
		"datamodel-code-generator": {
			Output: "datamodel-code-generator.py",
			Status: "datamodel-code-generator.status",
			Stderr: "datamodel-code-generator.stderr",
		},
		"go-jsonschema": {
			Output: "go-jsonschema.go",
			Status: "go-jsonschema.status",
			Stderr: "go-jsonschema.stderr",
		},
		"oapi-codegen": {
			Output: "oapi-codegen.go",
			Status: "oapi-codegen.status",
			Stderr: "oapi-codegen.stderr",
		},
	}
	if len(findings.Candidates) != len(files) {
		fatal(fmt.Errorf("candidate count = %d, want %d", len(findings.Candidates), len(files)))
	}
	result := summaryDocument{SchemaVersion: 1, Candidates: make([]summary, 0, len(findings.Candidates))}
	seen := make(map[string]bool, len(files))
	for _, candidate := range findings.Candidates {
		candidateFiles, exists := files[candidate.Name]
		if !exists || seen[candidate.Name] {
			fatal(fmt.Errorf("unexpected or duplicate candidate %q", candidate.Name))
		}
		seen[candidate.Name] = true
		exitCode, err := readExitCode(filepath.Join(*candidateDirectory, candidateFiles.Status))
		if err != nil {
			fatal(err)
		}
		if exitCode != candidate.ExpectedExit {
			fatal(fmt.Errorf("candidate %s exit = %d, want %d", candidate.Name, exitCode, candidate.ExpectedExit))
		}
		output, err := os.ReadFile(filepath.Join(*candidateDirectory, candidateFiles.Output))
		if errors.Is(err, os.ErrNotExist) && candidate.ExpectedExit != 0 {
			output = []byte{}
		} else if err != nil {
			fatal(fmt.Errorf("read candidate %s output: %w", candidate.Name, err))
		}
		stderr, err := os.ReadFile(filepath.Join(*candidateDirectory, candidateFiles.Stderr))
		if err != nil {
			fatal(fmt.Errorf("read candidate %s stderr: %w", candidate.Name, err))
		}
		if err := validateSemantics(candidate.Name, string(output), string(stderr)); err != nil {
			fatal(err)
		}
		digest := sha256.Sum256(output)
		result.Candidates = append(result.Candidates, summary{
			finding:      candidate,
			ExitCode:     exitCode,
			OutputBytes:  len(output),
			OutputSHA256: hex.EncodeToString(digest[:]),
		})
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*outputPath, append(data, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("contract_candidates=PASS candidates=%d\n", len(result.Candidates))
}

func readFindings(path string) (findingDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return findingDocument{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var document findingDocument
	if err := decoder.Decode(&document); err != nil {
		return findingDocument{}, err
	}
	if document.SchemaVersion != 1 {
		return findingDocument{}, errors.New("findings schema_version must be 1")
	}
	return document, nil
}

func readExitCode(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid candidate exit code in %s", filepath.Base(path))
	}
	return value, nil
}

func validateSemantics(name, output, stderr string) error {
	require := func(pattern string) error {
		if !strings.Contains(output, pattern) {
			return fmt.Errorf("candidate %s output is missing %q", name, pattern)
		}
		return nil
	}
	switch name {
	case "json-schema-to-typescript":
		for _, pattern := range []string{"note?: string | null", "sequence: number", "[k: string]: unknown"} {
			if err := require(pattern); err != nil {
				return err
			}
		}
	case "datamodel-code-generator":
		for _, pattern := range []string{"sequence: int", "note: str | None = None"} {
			if err := require(pattern); err != nil {
				return err
			}
		}
		if strings.Contains(output, "unknown_fields") {
			return errors.New("datamodel candidate unexpectedly emitted an unknown-field bag; update finding")
		}
	case "go-jsonschema":
		for _, pattern := range []string{"Sequence int", "type FixtureNote *string", "AdditionalProperties interface{}"} {
			if err := require(pattern); err != nil {
				return err
			}
		}
		if strings.Contains(output, "MarshalJSON") {
			return errors.New("go-jsonschema candidate unexpectedly emitted unknown-field marshaling; update finding")
		}
	case "oapi-codegen":
		if !strings.Contains(stderr, "unhandled Schema type") || !strings.Contains(stderr, "string null") {
			return errors.New("oapi-codegen rejection reason changed")
		}
	default:
		return fmt.Errorf("unsupported candidate %q", name)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
