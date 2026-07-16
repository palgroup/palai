package contracts_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(out))
}

func runMake(t *testing.T, target string) error {
	t.Helper()
	cmd := exec.Command("make", target)
	cmd.Dir = repoRoot(t)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func hashTree(t *testing.T, root string) string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	hasher := sha256.New()
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(hasher, "%s\n", relative)
		hasher.Write(data)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func TestGenerateIsDeterministic(t *testing.T) {
	if err := runMake(t, "generate"); err != nil {
		t.Fatalf("first generate: %v", err)
	}
	first := hashTree(t, filepath.Join(repoRoot(t), "packages/contracts"))
	if err := runMake(t, "generate"); err != nil {
		t.Fatalf("second generate: %v", err)
	}
	second := hashTree(t, filepath.Join(repoRoot(t), "packages/contracts"))
	if first != second {
		t.Fatal("generate is not deterministic")
	}
}

func TestCheckGeneratedFailsOnDrift(t *testing.T) {
	root := repoRoot(t)
	target := filepath.Join(root, "packages/contracts/ids.gen.go")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.WriteFile(target, original, 0o644) })
	if err := os.WriteFile(target, append(original, []byte("\n// drift\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runMake(t, "check-generated"); err == nil {
		t.Fatal("check-generated must fail on drift")
	}
}
