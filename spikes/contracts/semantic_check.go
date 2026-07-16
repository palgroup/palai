package contracts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

type languageResult struct {
	Name          string `json:"name"`
	NoteState     string `json:"note_state"`
	Status        string `json:"status"`
	Sequence      string `json:"sequence"`
	HasExtra      bool   `json:"has_extra"`
	HasFutureMeta bool   `json:"has_future_meta"`
	Encoded       string `json:"encoded"`
}

type corpusCase struct {
	Name          string
	NoteState     string
	Status        string
	HasExtra      bool
	HasFutureMeta bool
}

func locateContractRoot() (string, error) {
	command := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("locate repository root: %w", err)
	}
	return filepath.Join(strings.TrimSpace(string(output)), "spikes", "contracts"), nil
}

func runGeneratedLanguage(
	ctx context.Context,
	contractRoot string,
	language string,
	cases []corpusCase,
) ([]languageResult, error) {
	fixturePaths := make([]string, 0, len(cases))
	for _, testCase := range cases {
		fixturePaths = append(fixturePaths, filepath.Join(contractRoot, "fixtures", testCase.Name+".json"))
	}
	var output []byte
	var err error
	switch language {
	case "python":
		arguments := append([]string{filepath.Join(contractRoot, "generated", "python", "check.py")}, fixturePaths...)
		command := exec.CommandContext(ctx, "python3", arguments...)
		command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		output, err = command.CombinedOutput()
	case "typescript":
		tooling := filepath.Join(contractRoot, "tooling")
		tsconfig := filepath.Join(contractRoot, "generated", "typescript", "tsconfig.json")
		compile := exec.CommandContext(ctx, "pnpm", "--dir", tooling, "exec", "tsc", "--project", tsconfig)
		if compiled, compileErr := compile.CombinedOutput(); compileErr != nil {
			return nil, fmt.Errorf("compile generated TypeScript: %w: %s", compileErr, strings.TrimSpace(string(compiled)))
		}
		arguments := append([]string{filepath.Join(contractRoot, ".build", "typescript", "check.js")}, fixturePaths...)
		output, err = exec.CommandContext(ctx, "node", arguments...).CombinedOutput()
	default:
		return nil, fmt.Errorf("unsupported generated language %q", language)
	}
	if err != nil {
		return nil, fmt.Errorf("run generated %s checker: %w: %s", language, err, strings.TrimSpace(string(output)))
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	var results []languageResult
	if err := decoder.Decode(&results); err != nil {
		return nil, fmt.Errorf("decode generated %s result: %w", language, err)
	}
	if err := requireEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode generated %s result: %w", language, err)
	}
	return results, nil
}

func requireGeneratedLanguageRejection(
	ctx context.Context,
	contractRoot string,
	language string,
	fixturePath string,
) error {
	var command *exec.Cmd
	switch language {
	case "python":
		command = exec.CommandContext(
			ctx,
			"python3",
			filepath.Join(contractRoot, "generated", "python", "check.py"),
			fixturePath,
		)
		command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	case "typescript":
		tooling := filepath.Join(contractRoot, "tooling")
		tsconfig := filepath.Join(contractRoot, "generated", "typescript", "tsconfig.json")
		compile := exec.CommandContext(ctx, "pnpm", "--dir", tooling, "exec", "tsc", "--project", tsconfig)
		if output, err := compile.CombinedOutput(); err != nil {
			return fmt.Errorf("compile generated TypeScript: %w: %s", err, strings.TrimSpace(string(output)))
		}
		command = exec.CommandContext(
			ctx,
			"node",
			filepath.Join(contractRoot, ".build", "typescript", "check.js"),
			fixturePath,
		)
	default:
		return fmt.Errorf("unsupported generated language %q", language)
	}
	if output, err := command.CombinedOutput(); err == nil {
		return fmt.Errorf("generated %s checker accepted invalid corpus: %s", language, strings.TrimSpace(string(output)))
	}
	return nil
}

func decodeSemanticJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := requireEOF(decoder); err != nil {
		return nil, err
	}
	return value, nil
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func schemaWithoutIdentity(schema map[string]any) map[string]any {
	delete(schema, "$schema")
	delete(schema, "$id")
	return schema
}

func schemaFromOpenAPI(document map[string]any) (map[string]any, error) {
	components, ok := document["components"].(map[string]any)
	if !ok {
		return nil, errors.New("OpenAPI components are missing")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		return nil, errors.New("OpenAPI schemas are missing")
	}
	fixture, ok := schemas["Fixture"].(map[string]any)
	if !ok {
		return nil, errors.New("OpenAPI Fixture schema is missing")
	}
	return fixture, nil
}

func validateFixtureCorpus(value map[string]any) error {
	for _, name := range []string{"id", "status", "metadata", "sequence", "created_at"} {
		if _, exists := value[name]; !exists {
			return fmt.Errorf("required field %s is missing", name)
		}
	}
	if id, ok := value["id"].(string); !ok || id == "" {
		return errors.New("id must be a non-empty string")
	}
	if status, ok := value["status"].(string); !ok || status == "" {
		return errors.New("status must be a non-empty open string")
	}
	if _, ok := value["metadata"].(map[string]any); !ok {
		return errors.New("metadata must be an object")
	}
	if note, exists := value["note"]; exists && note != nil {
		if _, ok := note.(string); !ok {
			return errors.New("note must be string or null")
		}
	}
	sequence, ok := value["sequence"].(json.Number)
	if !ok || strings.ContainsAny(sequence.String(), ".eE") {
		return errors.New("sequence must be an integer")
	}
	integer, ok := new(big.Int).SetString(sequence.String(), 10)
	if !ok || integer.Sign() < 0 || integer.BitLen() > 63 {
		return errors.New("sequence must be a non-negative int64")
	}
	createdAt, ok := value["created_at"].(string)
	if !ok {
		return errors.New("created_at must be a string")
	}
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		return fmt.Errorf("created_at must be RFC3339: %w", err)
	}
	return nil
}

func generateProjection(ctx context.Context, contractRoot, outputRoot string) error {
	repositoryRoot := filepath.Dir(filepath.Dir(contractRoot))
	command := exec.CommandContext(
		ctx,
		"go",
		"run",
		"./spikes/contracts/generator",
		"-root",
		contractRoot,
		"-out",
		outputRoot,
	)
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("generate projection: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func compareDirectories(first, second string) error {
	firstFiles, err := directoryFiles(first)
	if err != nil {
		return err
	}
	secondFiles, err := directoryFiles(second)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(sortedKeys(firstFiles), sortedKeys(secondFiles)) {
		return fmt.Errorf("file sets differ: first=%v second=%v", sortedKeys(firstFiles), sortedKeys(secondFiles))
	}
	for path, firstData := range firstFiles {
		if !bytes.Equal(firstData, secondFiles[path]) {
			return fmt.Errorf("generated file differs: %s", path)
		}
	}
	return nil
}

func directoryFiles(root string) (map[string][]byte, error) {
	files := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("generated path is not a regular file: %s", entry.Name())
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = data
		return nil
	})
	return files, err
}

func sortedKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
