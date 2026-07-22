package extensions

import (
	"errors"
	"testing"
)

// TestUnknownHookPointRejected proves the hook create body accepts ONLY one of the five pinned points and
// rejects anything else (spec §28.17): a point outside the closed set is a typed reject BEFORE any write, so
// dead config (a hook wired to a point nothing fires) can never be stored.
func TestUnknownHookPointRejected(t *testing.T) {
	valid := `{"name":"guard","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"deny_tool"}}`
	if _, err := DecodeHookInput([]byte(valid)); err != nil {
		t.Fatalf("valid before_tool policy hook rejected: %v", err)
	}
	unknown := `{"name":"guard","hook_point":"before_everything","category":"observer","executor":"platform_inline","config":{"handler":"x"}}`
	if _, err := DecodeHookInput([]byte(unknown)); !errors.Is(err, ErrUnknownHookPoint) {
		t.Fatalf("unknown hook_point error = %v, want ErrUnknownHookPoint", err)
	}
}

// TestHookMatrixRejectsOutOfMatrixCategory pins the (category × point) matrix (spec §28.17): policy fires at
// before_tool/before_model/before_repository_publish; transform at before_tool/after_tool; observer at all
// five. A category paired with a point outside its row is a typed reject — a transform on before_model (no
// arguments/result to patch), or a policy on after_tool (the effect already ran), is never storable.
func TestHookMatrixRejectsOutOfMatrixCategory(t *testing.T) {
	ok := []string{
		`{"name":"a","hook_point":"before_tool","category":"transform","executor":"platform_inline","config":{"handler":"x"}}`,
		`{"name":"b","hook_point":"after_tool","category":"transform","executor":"platform_inline","config":{"handler":"x"}}`,
		`{"name":"c","hook_point":"before_model","category":"policy","executor":"platform_inline","config":{"handler":"x"}}`,
		`{"name":"d","hook_point":"before_repository_publish","category":"policy","executor":"platform_inline","config":{"handler":"x"}}`,
		`{"name":"e","hook_point":"on_terminal","category":"observer","executor":"platform_inline","config":{"handler":"x"}}`,
	}
	for _, body := range ok {
		if _, err := DecodeHookInput([]byte(body)); err != nil {
			t.Fatalf("in-matrix hook rejected: body=%s err=%v", body, err)
		}
	}
	bad := []string{
		`{"name":"a","hook_point":"before_model","category":"transform","executor":"platform_inline","config":{"handler":"x"}}`,       // no args/result to patch
		`{"name":"b","hook_point":"after_tool","category":"policy","executor":"platform_inline","config":{"handler":"x"}}`,            // effect already ran
		`{"name":"c","hook_point":"on_terminal","category":"policy","executor":"platform_inline","config":{"handler":"x"}}`,           // nothing to deny at terminal
		`{"name":"d","hook_point":"before_repository_publish","category":"transform","executor":"platform_inline","config":{"handler":"x"}}`, // no patch surface
	}
	for _, body := range bad {
		if _, err := DecodeHookInput([]byte(body)); !errors.Is(err, ErrHookMatrixViolation) {
			t.Fatalf("out-of-matrix hook error = %v, want ErrHookMatrixViolation: body=%s", err, body)
		}
	}
}

// TestHookInlineSecretRejected proves a credential can never ride the hook row inline: an unknown field
// (a raw `secret` / `bearer`) is rejected by json.DisallowUnknownFields — a signing credential is only ever
// a secret_ref HANDLE. This is the same strict-decode guard the tool/MCP registries enforce (spec §28.4).
func TestHookInlineSecretRejected(t *testing.T) {
	for _, body := range []string{
		`{"name":"a","hook_point":"before_tool","category":"policy","executor":"remote_http","config":{"url":"https://h.example/hook"},"secret":"sk-live-xxx"}`,
		`{"name":"b","hook_point":"before_tool","category":"policy","executor":"remote_http","config":{"url":"https://h.example/hook","bearer":"sk-xxx"}}`,
	} {
		if _, err := DecodeHookInput([]byte(body)); err == nil {
			t.Fatalf("inline-secret hook accepted, want reject: body=%s", body)
		}
	}
}

// TestHookRemoteRequiresSignedWiring proves a remote_http hook must carry a vettable https url AND a
// secret_ref handle (a signed transport needs a secret, spec §28.17). A remote hook without a url, or with an
// internal/downgraded url, or with no secret_ref, is a typed reject at create — never a run-time surprise.
func TestHookRemoteRequiresSignedWiring(t *testing.T) {
	if _, err := DecodeHookInput([]byte(`{"name":"a","hook_point":"before_tool","category":"policy","executor":"remote_http","config":{"url":"https://h.example/hook"},"secret_ref":"sref_x"}`)); err != nil {
		t.Fatalf("valid remote hook rejected: %v", err)
	}
	bad := []string{
		`{"name":"a","hook_point":"before_tool","category":"policy","executor":"remote_http","config":{"url":"https://h.example/hook"}}`,        // no secret_ref
		`{"name":"b","hook_point":"before_tool","category":"policy","executor":"remote_http","config":{},"secret_ref":"sref_x"}`,                // no url
		`{"name":"c","hook_point":"before_tool","category":"policy","executor":"remote_http","config":{"url":"http://10.0.0.1/hook"},"secret_ref":"sref_x"}`, // internal/downgrade
	}
	for _, body := range bad {
		if _, err := DecodeHookInput([]byte(body)); !errors.Is(err, ErrInvalidHookConfig) {
			t.Fatalf("bad remote hook error = %v, want ErrInvalidHookConfig: body=%s", err, body)
		}
	}
}

// TestHookInlineRequiresHandler proves a platform_inline hook must name a handler in config (the code-defined
// deterministic function it dispatches to); an inline hook with no handler is a typed reject at create.
func TestHookInlineRequiresHandler(t *testing.T) {
	if _, err := DecodeHookInput([]byte(`{"name":"a","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{}}`)); !errors.Is(err, ErrInvalidHookConfig) {
		t.Fatalf("inline hook with no handler error = %v, want ErrInvalidHookConfig", err)
	}
	if _, err := DecodeHookInput([]byte(`{"name":"a","hook_point":"before_tool","category":"policy","executor":"bogus_executor","config":{}}`)); !errors.Is(err, ErrInvalidHookExecutor) {
		t.Fatalf("bad executor error = %v, want ErrInvalidHookExecutor", err)
	}
}
