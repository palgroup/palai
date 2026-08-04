package uat

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A PUBLISHED RECORD IS NEVER REWRITTEN. When a shipped bundle's clause stops being true, the record that
// says so is a NEW record naming the old one — evidence/releases/* keeps the bytes it was published with.
// evidence/superseded/ is where those replacements live, in the row shape tool-execution-0.1.0's own
// SupersededLedger established: release + case + the clause falsified + why it is false now + the symbol
// the old reasoning RESTED ON + a claim that symbol is gone + the evidence for all of it.
//
// WHY IT IS A SEPARATE DIRECTORY RATHER THAN A ROW IN THAT LEDGER: the ledger is an INPUT to
// tool-execution-0.1.0's manifest, and its `ceilings_superseded` count is published. Appending to it would
// falsify that bundle's own checksum and force a regeneration, so a record about a LATER change cannot
// live inside an EARLIER release. The next release that publishes a superseded ledger should absorb what
// it finds here.
type supersededRecord struct {
	Release      string `json:"release"`
	Case         string `json:"case"`
	Clause       string `json:"clause"`
	NowFalse     string `json:"now_false"`
	RestedOn     string `json:"rested_on"`
	RestedOnGone bool   `json:"rested_on_gone"`
	Evidence     string `json:"evidence"`
}

// TestEverySupersededRecordNamesAPublishedClauseAndAGoneSymbol is the guard, and it is written to make the
// two ways this file could become decoration BOTH fail.
//
//	(1) A record could supersede something that was never published — an opinion dressed as a correction.
//	    So the release directory must exist, its manifest must parse, and the CASE the record names must be
//	    in it. A record about a case nobody shipped fails here.
//	(2) A record could claim a symbol is gone while it is still in the tree, which would leave the earlier
//	    ceiling standing and this record wrong in the other direction. So `rested_on` is swept for across
//	    the source of the repository and must not be found.
//
// The sweep deliberately does NOT read evidence/ — a published manifest may well name the thing that went,
// and that is the record doing its job, not a survival.
func TestEverySupersededRecordNamesAPublishedClauseAndAGoneSymbol(t *testing.T) {
	root := repoRootUntagged(t)
	dir := filepath.Join(root, "evidence", "superseded")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	rows := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var records []supersededRecord
		if err := json.Unmarshal(raw, &records); err != nil {
			t.Fatalf("%s does not decode as a superseded ledger: %v", entry.Name(), err)
		}
		if len(records) == 0 {
			t.Errorf("%s holds no rows; an empty ledger supersedes nothing", entry.Name())
		}
		for _, r := range records {
			rows++
			for field, value := range map[string]string{
				"release": r.Release, "case": r.Case, "clause": r.Clause,
				"now_false": r.NowFalse, "rested_on": r.RestedOn, "evidence": r.Evidence,
			} {
				if strings.TrimSpace(value) == "" {
					t.Errorf("%s: a row names no %s, so nothing about it can be re-measured", entry.Name(), field)
				}
			}
			if !r.RestedOnGone {
				t.Errorf("%s: the supersession of %s/%s names %q as what its old reasoning rested on and does "+
					"NOT claim it is gone — then the earlier clause may still stand, and this row is an opinion "+
					"about a published record rather than a supersession of it",
					entry.Name(), r.Release, r.Case, r.RestedOn)
				continue
			}
			assertPublishedCase(t, root, entry.Name(), r)
			assertSymbolGone(t, root, entry.Name(), r)
		}
	}
	// NON-VACUITY: this whole test is a loop over a directory, so an empty or renamed directory would make
	// every assertion above unreachable and the test would pass having measured nothing.
	if rows == 0 {
		t.Fatal("evidence/superseded holds no rows at all; this guard measured nothing")
	}
}

// assertPublishedCase proves the row supersedes something that was actually shipped.
func assertPublishedCase(t *testing.T, root, file string, r supersededRecord) {
	t.Helper()
	manifest := filepath.Join(root, "evidence", "releases", r.Release, "manifest.json")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Errorf("%s: supersedes release %q, which has no published manifest: %v", file, r.Release, err)
		return
	}
	var m struct {
		Release string `json:"release"`
		Cases   []struct {
			ID string `json:"id"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Errorf("%s: %s/manifest.json does not decode: %v", file, r.Release, err)
		return
	}
	if m.Release != r.Release {
		t.Errorf("%s: names release %q but that manifest calls itself %q", file, r.Release, m.Release)
	}
	for _, c := range m.Cases {
		if c.ID == r.Case {
			return
		}
	}
	t.Errorf("%s: supersedes case %q of %s, and that release publishes no such case — a record cannot "+
		"correct a claim nobody made", file, r.Case, r.Release)
}

// assertSymbolGone sweeps the repository's SOURCE for the symbol the superseded reasoning rested on. It is
// a walk rather than a list, because the thing being proven is an ABSENCE and a list only sees what it
// already knows to look at.
func assertSymbolGone(t *testing.T, root, file string, r supersededRecord) {
	t.Helper()
	skipDir := map[string]bool{
		".git": true, "node_modules": true, "evidence": true, ".next": true, "dist": true,
	}
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner is not evidence of survival; the named-file check below is
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".sql", ".sh", ".ts", ".tsx", ".yaml", ".yml":
		default:
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(body), r.RestedOn) {
			rel, _ := filepath.Rel(root, path)
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: sweep for %q: %v", file, r.RestedOn, err)
	}
	if len(found) > 0 {
		t.Errorf("%s: claims %q is gone, but it is still in %v — the earlier clause may still stand",
			file, r.RestedOn, found)
	}
}
