package coordinator

import "testing"

// TestConfigPolicyAllowlistIsTypedNotFallback proves the project allowlist denies an
// out-of-list model or tool outright, never narrowing to an allowed value (spec §9.3; SES-008
// unit half). An empty allowlist is unrestricted, so an unconfigured project permits any change.
func TestConfigPolicyAllowlistIsTypedNotFallback(t *testing.T) {
	policy := ConfigPolicy{
		AllowedModels: []string{"model-alpha", "model-beta"},
		AllowedTools:  []string{"palai.conformance.math.add"},
	}

	if !policy.AllowModel("model-beta") {
		t.Fatal("an allowlisted model must be permitted")
	}
	if policy.AllowModel("model-forbidden") {
		t.Fatal("a model outside the allowlist must be denied, not silently narrowed")
	}
	if !policy.AllowModel("") {
		t.Fatal("an empty (tools-only) model change must be permitted")
	}
	if bad := policy.DeniedTool([]string{"palai.conformance.math.add"}); bad != "" {
		t.Fatalf("an allowlisted tool must be permitted, got denied %q", bad)
	}
	if bad := policy.DeniedTool([]string{"palai.fs.write"}); bad != "palai.fs.write" {
		t.Fatalf("a tool outside the allowlist must be denied by name, got %q", bad)
	}

	// An empty allowlist is unrestricted (the default project, NULL config_policy).
	open := ConfigPolicy{}
	if !open.AllowModel("anything") || open.DeniedTool([]string{"anything"}) != "" {
		t.Fatal("an empty policy must be unrestricted")
	}
}
