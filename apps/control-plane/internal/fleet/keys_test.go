package fleet

// The Docker-free half of the pool enrolment key: what the VALUE is, what is stored instead of it, and
// the structural guard that both credential comparisons in this tree are constant-time.
//
// It is `package fleet` rather than `fleet_test` because the three things worth pinning here are
// unexported by design — the value generator, the digest, and the published prefix length. Exporting
// them so a test could reach them would put a credential mint on the package's public surface.

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestAKeyValueIsHighEntropyAndDistinctlyPrefixed pins the shape of the credential: a `rpk_` prefix so
// it cannot be mistaken for an API key (`sk_`), 24 random bytes behind it, and a fresh value every time.
func TestAKeyValueIsHighEntropyAndDistinctlyPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		value, err := newKeyValue()
		if err != nil {
			t.Fatalf("mint value: %v", err)
		}
		if !strings.HasPrefix(value, "rpk_") {
			t.Fatalf("value %q does not carry the rpk_ prefix; a pool key pasted into an Authorization header must be recognisable as the wrong kind of credential", value)
		}
		// 4 characters of prefix plus 48 hex characters for 24 bytes.
		if len(value) != 4+48 {
			t.Fatalf("value length = %d, want %d (24 random bytes hex-encoded behind the prefix)", len(value), 4+48)
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(value, "rpk_")); err != nil {
			t.Fatalf("value body is not hex: %v", err)
		}
		if seen[value] {
			t.Fatalf("newKeyValue returned a repeat after %d mints", i)
		}
		seen[value] = true
	}
}

// TestTheStoredVerifierIsTheDigestAndNotTheValue pins what the column holds. It is a small test for a
// property that is impossible to check by reading a database dump after the fact: by then the value is
// gone and there is nothing to compare it to.
func TestTheStoredVerifierIsTheDigestAndNotTheValue(t *testing.T) {
	value, err := newKeyValue()
	if err != nil {
		t.Fatalf("mint value: %v", err)
	}
	stored := hashKey(value)
	sum := sha256.Sum256([]byte(value))
	if stored != hex.EncodeToString(sum[:]) {
		t.Fatalf("hashKey(value) = %q, want the sha256 hex digest", stored)
	}
	if strings.Contains(stored, value) || strings.Contains(stored, strings.TrimPrefix(value, "rpk_")) {
		t.Fatal("the stored verifier contains the value it verifies")
	}
	// The digest is deterministic (a redemption resolves by it) and a different value digests differently.
	if hashKey(value) != stored {
		t.Fatal("hashKey is not deterministic; a minted key could never be redeemed")
	}
	other, err := newKeyValue()
	if err != nil {
		t.Fatalf("mint value: %v", err)
	}
	if hashKey(other) == stored {
		t.Fatal("two distinct values share a digest")
	}
}

// TestThePublishedPrefixIsShortEnoughToBeUseless pins the listing bargain: 8 characters is enough to
// tell two keys apart and — with 4 of them being the fixed `rpk_` — leaves 188 bits unpublished.
func TestThePublishedPrefixIsShortEnoughToBeUseless(t *testing.T) {
	if keyPrefixLength != 8 {
		t.Fatalf("keyPrefixLength = %d; the listing publishes that many characters of a credential and 8 is the reviewed number", keyPrefixLength)
	}
	value, err := newKeyValue()
	if err != nil {
		t.Fatalf("mint value: %v", err)
	}
	published := value[:keyPrefixLength]
	if len(value)-len(published) < 40 {
		t.Fatalf("publishing %q leaves only %d characters unpublished", published, len(value)-len(published))
	}
}

// TestEveryPresentedCredentialIsComparedInConstantTime is a STRUCTURAL guard over the two files that
// compare a presented enrolment credential, and it is structural for a reason: a timing property cannot
// be asserted from a wall clock in a test that has to pass on a loaded 16 GiB laptop.
//
// It requires each comparison site to CALL crypto/subtle.ConstantTimeCompare, and it separately refuses
// any `==`/`!=` whose operands mention the presented value. The second half is what makes it a guard
// rather than a decoration: adding a subtle call while leaving a `!=` beside it would otherwise pass.
//
// FileEnrollmentTokens.Consume was exactly that `!=` until E24 T3 (§3.6 D6 measured it), so this is a
// fence around a regression that has already happened once.
func TestEveryPresentedCredentialIsComparedInConstantTime(t *testing.T) {
	sites := []struct {
		file, function string
		credentials    []string
	}{
		{"keys.go", "RedeemPoolKey", []string{"presented", "digest"}},
		{"../execution/local_credentials.go", "Consume", []string{"token", "raw"}},
	}
	for _, site := range sites {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, site.file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", site.file, err)
		}
		var fn *ast.FuncDecl
		for _, decl := range parsed.Decls {
			if candidate, ok := decl.(*ast.FuncDecl); ok && candidate.Name.Name == site.function {
				fn = candidate
			}
		}
		if fn == nil {
			t.Fatalf("%s: no function named %s — this guard names its subjects and a rename must break it, not skip it", site.file, site.function)
		}
		constantTime := false
		ast.Inspect(fn, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CallExpr:
				if selector, ok := n.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "subtle" && selector.Sel.Name == "ConstantTimeCompare" {
						constantTime = true
					}
				}
			case *ast.BinaryExpr:
				if n.Op != token.EQL && n.Op != token.NEQ {
					return true
				}
				// An equality whose operands mention the credential is the bug this guard exists for. Two
				// forms are allowed and both are named rather than inferred: a comparison against the empty
				// string (the length check every implementation needs, not a value comparison), and the
				// `subtle.ConstantTimeCompare(...) != 1` idiom, which IS the sanctioned comparison.
				if mentionsCredential(n, site.credentials) && !comparesToEmptyString(n) && !isConstantTimeIdiom(n) {
					t.Errorf("%s: %s compares a presented credential with %s — use subtle.ConstantTimeCompare",
						site.file, site.function, n.Op)
				}
			}
			return true
		})
		if !constantTime {
			t.Errorf("%s: %s does not call subtle.ConstantTimeCompare", site.file, site.function)
		}
	}
}

// TestThePoolKeySurfaceHasAProductionCaller is the fence this repository has earned. E19 T9's exit gate
// found `CreateSlackConnection` fully built, fully tested and reachable from NOTHING — no route, no
// binary — and E23 T2 found `DecideToolApproval` in the same state. A credential surface that no
// production path constructs is a security feature that does not exist, and every test in this package
// would still pass.
//
// It names three sites, because three is what a working pool key needs: the store is CONSTRUCTED, it is
// handed to the gateway (so a presented key is resolved at enrolment), and it is handed to the router (so
// an operator can mint and revoke one). Any two of the three is a half-built feature.
func TestThePoolKeySurfaceHasAProductionCaller(t *testing.T) {
	const mainFile = "../../cmd/palai-control-plane/main.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, mainFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", mainFile, err)
	}
	// Each site is identified by the call the composition root has to make, matched on the selector's
	// NAME rather than on a line number or a source substring — a rename must break this, and a
	// reformat must not.
	sites := map[string]bool{
		"NewPoolEnrollmentKeys": false, // the store is constructed
		"SetPoolKeys":           false, // the gateway resolves presented keys
		"NewRegistryAPI":        false, // the router can mint and revoke one
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if _, tracked := sites[fn.Sel.Name]; tracked {
				sites[fn.Sel.Name] = true
			}
		case *ast.Ident:
			if _, tracked := sites[fn.Name]; tracked {
				sites[fn.Name] = true
			}
		}
		return true
	})
	for site, wired := range sites {
		if !wired {
			t.Errorf("%s: nothing calls %s — the pool key surface is unreachable from the shipped binary, which is how two fully-tested surfaces shipped dead in this tree", mainFile, site)
		}
	}
}

// mentionsCredential reports whether either side of a comparison names one of the credential-bearing
// identifiers. It walks the operand rather than matching an identifier at the top, so
// `strings.TrimSpace(string(raw)) != token` — the exact expression this guard was written for — is caught.
func mentionsCredential(expr *ast.BinaryExpr, names []string) bool {
	found := false
	for _, side := range []ast.Expr{expr.X, expr.Y} {
		ast.Inspect(side, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok {
				for _, name := range names {
					if ident.Name == name {
						found = true
					}
				}
			}
			return true
		})
	}
	return found
}

// isConstantTimeIdiom reports whether the comparison is `subtle.ConstantTimeCompare(...) != 1` — the
// sanctioned form, whose operands necessarily mention the credential.
func isConstantTimeIdiom(expr *ast.BinaryExpr) bool {
	found := false
	for _, side := range []ast.Expr{expr.X, expr.Y} {
		ast.Inspect(side, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "subtle" && selector.Sel.Name == "ConstantTimeCompare" {
					found = true
				}
			}
			return true
		})
	}
	return found
}

// comparesToEmptyString reports whether one side is the literal "". Checking a credential for emptiness
// is a length check, not a value comparison, and every implementation here needs one.
func comparesToEmptyString(expr *ast.BinaryExpr) bool {
	for _, side := range []ast.Expr{expr.X, expr.Y} {
		if literal, ok := side.(*ast.BasicLit); ok && literal.Kind == token.STRING && literal.Value == `""` {
			return true
		}
	}
	return false
}
