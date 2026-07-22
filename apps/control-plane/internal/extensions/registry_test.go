package extensions

import (
	"errors"
	"testing"
)

// TestDecodeToolRevisionInputRejectsUnknownField proves the tool-revision body accepts ONLY the enforced
// executor-config subset and rejects anything else via json.DisallowUnknownFields — dead/unsupported
// config is never silently stored (honest naming, spec §28.4). A credential is a secret_ref HANDLE; a
// raw `credential`/`api_key` field is rejected here, so a secret can never ride the row inline.
func TestDecodeToolRevisionInputRejectsUnknownField(t *testing.T) {
	valid := `{"executor":"control_plane","description":"echo","input_schema":{"type":"object"},"replay_class":"pure","timeout_ms":1000,"secret_ref":"sref_x"}`
	if _, err := DecodeToolRevisionInput([]byte(valid)); err != nil {
		t.Fatalf("valid subset rejected: %v", err)
	}
	rejected := map[string]string{
		"inline credential": `{"executor":"remote_http","credential":"sk-live-xxx"}`,
		"inline api_key":    `{"executor":"remote_http","api_key":"xxx"}`,
		"arbitrary field":   `{"executor":"control_plane","surprise":true}`,
	}
	for name, body := range rejected {
		if _, err := DecodeToolRevisionInput([]byte(body)); !errors.Is(err, ErrUnknownField) {
			t.Errorf("%s: err = %v, want ErrUnknownField", name, err)
		}
	}
}

// TestCanonicalNameValidationAndShortName pins the publisher.namespace.tool contract: exactly three
// non-empty ASCII segments within the length bound; the model-visible short name is deterministically the
// LAST segment (no auto-suffix). A malformed name is a typed reject BEFORE any write.
func TestCanonicalNameValidationAndShortName(t *testing.T) {
	short, err := validateCanonicalName("acme.search.fetch")
	if err != nil {
		t.Fatalf("valid canonical rejected: %v", err)
	}
	if short != "fetch" {
		t.Fatalf("model-visible short name = %q, want the deterministic last segment %q", short, "fetch")
	}
	bad := map[string]string{
		"two segments":  "acme.fetch",
		"four segments": "acme.search.sub.fetch",
		"empty segment": "acme..fetch",
		"non-ascii":     "acme.search.fetché",
		"too long":      "acme.search." + longName(),
	}
	for name, canonical := range bad {
		if _, err := validateCanonicalName(canonical); !errors.Is(err, ErrInvalidCanonicalName) {
			t.Errorf("%s (%q): err = %v, want ErrInvalidCanonicalName", name, canonical, err)
		}
	}
}

func longName() string {
	b := make([]byte, 200)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

// TestRevisionDigestDeterministic proves the digest is a pure content address: identical decoded input
// yields an identical digest (so an equivalent revision addresses identically and a published revision
// is verifiably frozen), and a changed field changes it.
func TestRevisionDigestDeterministic(t *testing.T) {
	a, _ := DecodeToolRevisionInput([]byte(`{"executor":"control_plane","description":"d","input_schema":{"type":"object"},"replay_class":"pure"}`))
	b, _ := DecodeToolRevisionInput([]byte(`{"executor":"control_plane","description":"d","input_schema":{"type":"object"},"replay_class":"pure"}`))
	if revisionDigest(a) != revisionDigest(b) {
		t.Fatal("identical revision input produced different digests")
	}
	c, _ := DecodeToolRevisionInput([]byte(`{"executor":"control_plane","description":"CHANGED","input_schema":{"type":"object"},"replay_class":"pure"}`))
	if revisionDigest(a) == revisionDigest(c) {
		t.Fatal("a changed description did not change the digest")
	}
}

// TestOverrideOnlyStricter proves a set-pin override may only TIGHTEN a declared limit: an override
// timeout above the declared ceiling is rejected; equal or below (or bounding a previously-unbounded
// declaration) is accepted (spec §28.4 approval-only-stricter).
func TestOverrideOnlyStricter(t *testing.T) {
	declared := 1000
	// > declared → reject.
	if err := checkOverrideStricter(map[string]any{"timeout_ms": float64(2000)}, &declared); !errors.Is(err, ErrOverrideNotStricter) {
		t.Fatalf("override 2000 > declared 1000: err = %v, want ErrOverrideNotStricter", err)
	}
	// ≤ declared → accept.
	if err := checkOverrideStricter(map[string]any{"timeout_ms": float64(500)}, &declared); err != nil {
		t.Fatalf("override 500 ≤ declared 1000 rejected: %v", err)
	}
	// Bounding a previously-unbounded (NULL) declaration is stricter → accept.
	if err := checkOverrideStricter(map[string]any{"timeout_ms": float64(500)}, nil); err != nil {
		t.Fatalf("override bounding an unbounded declaration rejected: %v", err)
	}
}
