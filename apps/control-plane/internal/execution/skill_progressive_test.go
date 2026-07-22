package execution

import (
	"strings"
	"testing"
)

// TestSkillMetadataProgressivelyLoadedNotFullBody pins progressive loading (spec §28.16): the system
// message prepended to a provider request carries a skill's METADATA (name + description + workspace
// path) but NEVER its body. The body reaches the model only on-demand via a file tool read of Path —
// the pin (SkillRef) carries no body field, so the message cannot leak one by construction.
func TestSkillMetadataProgressivelyLoadedNotFullBody(t *testing.T) {
	// The body text a malicious/large skill might carry — it must NOT appear in the context message.
	const bodyText = "SECRET_BODY_INSTRUCTIONS_do_the_thing"
	skills := []SkillRef{{
		Name:        "commit-convention",
		Description: "write conventional commit messages",
		Digest:      "sha256:abc123",
		Path:        ".palai/skills/commit-convention/SKILL.md",
	}}

	msg := skillContextMessage(skills)
	if msg.Role != "system" {
		t.Fatalf("skill context message role = %q, want system", msg.Role)
	}
	for _, want := range []string{"commit-convention", "write conventional commit messages", ".palai/skills/commit-convention/SKILL.md"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("skill context message missing metadata %q; got %q", want, msg.Content)
		}
	}
	if strings.Contains(msg.Content, bodyText) {
		t.Fatalf("skill context message leaked the body %q — progressive loading requires metadata only", bodyText)
	}

	// A skill-less run prepends nothing: the zero Message (empty content), so the provider request is
	// bit-identical to the pre-skills path.
	if empty := skillContextMessage(nil); empty.Content != "" || empty.Role != "" {
		t.Fatalf("skill context message for no skills = %+v, want the zero Message", empty)
	}
}
