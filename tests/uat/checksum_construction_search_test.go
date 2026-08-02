package uat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// preCorrectionChecksums pins the SIX checksum values E18 T8 replaced, so the verdict this task reached about
// each one is reproducible FROM THE TREE rather than from a scratchpad script that no longer exists.
//
// The two verdicts are NOT the same, and conflating them was the honest-naming defect this file closes:
//
//   - automation-0.1.0's four values reproduce from NOTHING in the declared search space below (the E11
//     deferred finding: FABRICATED — a sha256:<64 hex> shape the old verifier waved through).
//   - recovery-0.1.0's two values DO reproduce, exactly and immediately, from a real construction:
//     sha256(case_id | release-family | kind). They were never fabricated; they were authored with a
//     self-consistent construction that differs from the one its journey writer uses, so the bundle could not
//     recompute against its own generator. E18 T8 RENORMALIZES them onto that generator's hashParts form.
//
// Two independent cases matching one structural pattern is not a hash collision, which is what makes the
// recovery finding a construction-mismatch rather than a fabrication.
var preCorrectionChecksums = map[string]map[string]string{
	"automation-0.1.0": {
		"AUT-001": "sha256:72c3baae78e6894b26d78496122b63793821653edab3ebe76a071cc4b3f74e0e",
		"AUT-007": "sha256:af8c500e1f46f56b299c5f3868cdc9b70aae1a0ab1142f6270ca5821b4c97e7d",
		"AUT-013": "sha256:442dd6370d9a0c63c97fe5d8526d6b987100059ca9a2dd543f932664313fe86c",
		"ENG-004": "sha256:78a4396eb7f09be419e0461c4517fbf403b93b34d7db0ab7ce1045aa9b106eaf",
	},
	"recovery-0.1.0": {
		"ENG-004": "sha256:01a774771a585ff2dc16f99ddeaf675bbb280c5f0fcdc1dcaf87a593d539e181",
		"REP-006": "sha256:e1ed06ce2809f80b43a852435deb18589d7055e9a8aa05da2a1630b75b4c52f4",
	},
}

// hashRaw is sha256 over the exact bytes given, in the manifest's "sha256:<hex>" form. It is deliberately NOT
// hashParts: hashParts NUL-terminates every part, and the whole point of the search below is to cover join
// constructions OTHER than the generator's.
func hashRaw(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestPreCorrectionRecoveryValuesReproduce is the exact, load-bearing pin of recovery-0.1.0's TRUE history —
// the fact its checksum_note states. Both replaced values are sha256 of the case id, the release family and the
// case's kind joined by "|", with NO trailing NUL:
//
//	sha256("ENG-004|recovery|compatible_checkpoint") = 01a77477…e181   (the case's own recovery_proof.level)
//	sha256("REP-006|recovery|push-once")             = e1ed06ce…c52f4
//
// A construction no generator in this tree produces — hence the renormalization — but a REAL one. If this test
// ever fails, the note is lying and must be rewritten.
func TestPreCorrectionRecoveryValuesReproduce(t *testing.T) {
	for _, tc := range []struct{ id, kind, want string }{
		{"ENG-004", "compatible_checkpoint", preCorrectionChecksums["recovery-0.1.0"]["ENG-004"]},
		{"REP-006", "push-once", preCorrectionChecksums["recovery-0.1.0"]["REP-006"]},
	} {
		construction := strings.Join([]string{tc.id, "recovery", tc.kind}, "|")
		if got := hashRaw(construction); got != tc.want {
			t.Errorf("sha256(%q) = %s, want the committed pre-correction value %s", construction, got, tc.want)
		}
	}
	// The kind for ENG-004 is that case's OWN committed recovery_proof.level — not a value chosen here.
	var m evidenceManifest
	if err := json.Unmarshal(readBundle(t, "recovery-0.1.0"), &m); err != nil {
		t.Fatalf("decode recovery-0.1.0: %v", err)
	}
	for _, c := range m.Cases {
		if c.ID == "ENG-004" {
			if c.RecoveryProof == nil {
				t.Fatal("recovery-0.1.0 ENG-004 carries no recovery proof")
			}
			if c.RecoveryProof.Level != "compatible_checkpoint" {
				t.Errorf("ENG-004 recovery_proof.level = %q, but the pre-correction construction hashed %q",
					c.RecoveryProof.Level, "compatible_checkpoint")
			}
			return
		}
	}
	t.Fatal("recovery-0.1.0 carries no ENG-004 case")
}

// TestCorrectedChecksumsRecompute pins the fix itself: all six corrected values recompute from the construction
// their bundle's GENERATOR uses (hashParts, NUL-terminated), which is what makes the bundles verifiable against
// their own writers. Flip any one and this fails alongside the sweep.
func TestCorrectedChecksumsRecompute(t *testing.T) {
	for release, cases := range preCorrectionChecksums {
		var m evidenceManifest
		if err := json.Unmarshal(readBundle(t, release), &m); err != nil {
			t.Fatalf("decode %s: %v", release, err)
		}
		found := 0
		for _, c := range m.Cases {
			old, corrected := cases[c.ID]
			if !corrected {
				continue
			}
			found++
			if c.Checksum == old {
				t.Errorf("%s/%s still carries its pre-correction checksum %s", release, c.ID, old)
			}
			parts := caseChecksumParts(m, c)
			if parts == nil {
				t.Errorf("%s/%s resolves no canonical surface", release, c.ID)
				continue
			}
			if want := hashParts(parts...); c.Checksum != want {
				t.Errorf("%s/%s checksum %s does not recompute from %v (want %s)", release, c.ID, c.Checksum, parts, want)
			}
		}
		if found != len(cases) {
			t.Errorf("%s: pinned %d corrected cases but the manifest carries %d of them", release, len(cases), found)
		}
	}
}

// checksumSearchSeparators is the DECLARED join vocabulary of the search below. Beyond these, the search covers
// the generator's own NUL-TERMINATED form (hashParts) as a ninth join mode.
var checksumSearchSeparators = []string{"", "|", ":", "/", "-", " ", "_", "\x00"}

// checksumSearchTags is the DECLARED kind vocabulary: every kind tag any generator or historical construction
// in this tree is known to append, plus the release families. Without these the search is construction-blind —
// the first pass at this task searched only the manifest's own strings and therefore MISSED recovery's real
// construction, which is why "did not reproduce" now ships as a test with a positive control.
var checksumSearchTags = []string{
	"dedupe", "occurrence", "callback", "advertising", "skill", "crash-isolation", "recovery", "push-once",
	"automation", "extensibility", "compatible_checkpoint", "external-receipt", "e2e-deterministic",
}

// checksumSearchVocabulary is the DECLARED term vocabulary for one bundle: every string leaf and object key in
// its manifest, every token inside those strings, the kind/family tags, and the pre-correction checksum values
// themselves (so a hash-CHAINED construction is covered too).
//
// The three E18 T8 fields are excluded by design: `checksum` values are the search's OUTPUTS, and
// `checksum_surface`/`checksum_note` did not exist when these values were authored, so feeding this task's own
// prose back in as candidate input would be anachronistic.
func checksumSearchVocabulary(t *testing.T, release string) []string {
	t.Helper()
	var tree any
	if err := json.Unmarshal(readBundle(t, release), &tree); err != nil {
		t.Fatalf("decode %s: %v", release, err)
	}
	excluded := map[string]bool{"checksum": true, "checksum_surface": true, "checksum_note": true}
	set := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch node := v.(type) {
		case string:
			set[node] = true
		case []any:
			for _, e := range node {
				walk(e)
			}
		case map[string]any:
			for k, e := range node {
				set[k] = true
				if !excluded[k] {
					walk(e)
				}
			}
		}
	}
	walk(tree)
	// Tokens inside strings: a construction may hash one field of a prose assertion, not the whole line.
	for s := range set {
		for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
			return strings.ContainsRune(" |:/,.()=;\"\n\t", r)
		}) {
			set[tok] = true
		}
	}
	for _, tag := range checksumSearchTags {
		set[tag] = true
	}
	for _, cases := range preCorrectionChecksums {
		for _, v := range cases {
			set[v] = true
		}
	}
	terms := make([]string, 0, len(set))
	for s := range set {
		terms = append(terms, s)
	}
	sort.Strings(terms) // deterministic candidate order, so a hit is reproducible
	return terms
}

// searchChecksumConstructions enumerates the declared candidate space over terms — ordered products WITH
// repetition of length 1..3, joined by each of checksumSearchSeparators, plus the generator's NUL-terminated
// hashParts form — and returns every target it reproduces, keyed by target value, with the construction that
// produced it. It also returns the number of candidates hashed.
func searchChecksumConstructions(terms []string, targets map[string]string) (map[string]string, uint64) {
	// Index the targets by RAW digest: a [32]byte map key means the ~200M-candidate hot loop allocates
	// nothing per candidate (no "sha256:"+hex string, no concatenated preimage string). Same space as the
	// obvious string version, ~5x the throughput.
	byDigest := map[[32]byte]string{}
	for sum := range targets {
		raw, err := hex.DecodeString(strings.TrimPrefix(sum, "sha256:"))
		if err != nil || len(raw) != 32 {
			continue
		}
		byDigest[[32]byte(raw)] = sum
	}
	var (
		mu         sync.Mutex
		hits       = map[string]string{}
		candidates atomic.Uint64
		next       atomic.Int64
		wg         sync.WaitGroup
	)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := uint64(0)
			// probe hashes the candidate PREIMAGE bytes and records a reproduced target. The recorded
			// construction is the exact preimage, quoted — for the generator's NUL-terminated form that
			// prints as "a\x00b\x00", which says precisely what was hashed.
			probe := func(preimage []byte) {
				local++
				sum := sha256.Sum256(preimage)
				target, ok := byDigest[sum]
				if !ok {
					return
				}
				mu.Lock()
				if _, seen := hits[target]; !seen {
					hits[target] = fmt.Sprintf("sha256(%q)", preimage)
				}
				mu.Unlock()
			}
			pre, buf := make([]byte, 0, 1024), make([]byte, 0, 1024)
			for {
				i := int(next.Add(1)) - 1
				if i >= len(terms) {
					break
				}
				a := terms[i]
				probe(append(buf[:0], a...))            // length 1, raw
				probe(append(append(buf[:0], a...), 0)) // length 1, NUL-terminated (hashParts)
				for _, b := range terms {
					for _, sep := range checksumSearchSeparators {
						pre = append(pre[:0], a...)
						pre = append(pre, sep...)
						pre = append(pre, b...)
						probe(pre) // length 2, joined by sep
						pre = append(pre, sep...)
						for _, c := range terms {
							buf = append(buf[:0], pre...)
							buf = append(buf, c...)
							probe(buf) // length 3, joined by sep
						}
					}
					pre = append(pre[:0], a...)
					pre = append(pre, 0)
					pre = append(pre, b...)
					pre = append(pre, 0)
					probe(pre) // length 2, hashParts
					for _, c := range terms {
						buf = append(buf[:0], pre...)
						buf = append(buf, c...)
						buf = append(buf, 0)
						probe(buf) // length 3, hashParts
					}
				}
			}
			candidates.Add(local)
		}()
	}
	wg.Wait()
	return hits, candidates.Load()
}

// TestPreCorrectionChecksumConstructionSearch is what GROUNDS this task's two different verdicts (plan §2
// honest-naming). It runs ONE declared search over both corrected bundles:
//
//   - recovery-0.1.0's two pre-correction values MUST be found. They are the POSITIVE CONTROL: a search that
//     cannot reproduce a value known to be reproducible proves nothing about a value it fails to reproduce.
//     This is exactly the check the first pass lacked — its space was NUL-terminated-only and permutation
//     without repetition, so it missed a real construction and mislabelled it fabricated.
//   - automation-0.1.0's four pre-correction values MUST NOT be found. Since the same space, on the same day,
//     over the same construction families, DOES reproduce recovery's two, "reproduces nothing" is a supported
//     verdict for automation rather than an untested assumption.
//
// The candidate space is exactly what checksumSearchVocabulary + checksumSearchSeparators declare. The claim is
// bounded by that space and nothing wider — no search proves a negative absolutely, and the note in each
// manifest says only what this test checks.
func TestPreCorrectionChecksumConstructionSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive construction search (~10s): skipped under -short")
	}
	for _, release := range []string{"recovery-0.1.0", "automation-0.1.0"} {
		mustReproduce := release == "recovery-0.1.0"
		targets := map[string]string{}
		for id, sum := range preCorrectionChecksums[release] {
			targets[sum] = id
		}
		terms := checksumSearchVocabulary(t, release)
		hits, candidates := searchChecksumConstructions(terms, targets)
		t.Logf("SEARCH %-18s %d terms x %d separators (+hashParts), lengths 1..3 with repetition = %d candidates; %d/%d targets reproduced",
			release, len(terms), len(checksumSearchSeparators), candidates, len(hits), len(targets))
		for sum, construction := range hits {
			t.Logf("  reproduced %s (%s) = %s", targets[sum], sum[:19]+"…", construction)
		}
		switch {
		case mustReproduce && len(hits) != len(targets):
			t.Errorf("%s: only %d of %d pre-correction values reproduced — the search has gone construction-blind, "+
				"so its 0-hit result for automation-0.1.0 can no longer be trusted either", release, len(hits), len(targets))
		case !mustReproduce && len(hits) != 0:
			t.Errorf("%s: %d pre-correction value(s) DID reproduce %v — they were not fabricated and the "+
				"manifest's checksum_note must be rewritten to the construction found", release, len(hits), hits)
		}
	}
}
