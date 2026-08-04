package execution

import (
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/toolset"
)

// TestThePlatformTextNamesNoTool is the anti-duplication guard. A tool's description lives on the
// tool and already reaches the model through the advertisement seam; naming one here would create a
// second copy that drifts the day the tool changes — the defect this tree keeps paying for. The
// guidance may say WHAT to do ("search before you read") and never WHICH tool does it.
func TestThePlatformTextNamesNoTool(t *testing.T) {
	for _, name := range toolset.All() {
		if strings.Contains(platformInstructions, name) {
			t.Errorf("the platform text names the tool %q; tool text belongs on the tool", name)
		}
	}
	if strings.Contains(platformInstructions, "palai.") {
		t.Error("the platform text carries a palai.* identifier; it must describe work, not name plumbing")
	}
}

// TestThePlatformTextIsSubstantial — the state this replaced was twenty-seven words of protocol. A
// word count is a crude guard, and it is the one that would have caught what shipped for months.
func TestThePlatformTextIsSubstantial(t *testing.T) {
	if words := len(strings.Fields(platformInstructions)); words < 150 {
		t.Errorf("the platform text is %d words; the engine-only state it replaced was 27", words)
	}
}

// TestThePlatformTextPrescribesNoCharacter is the guard for the rule that produced this text's
// second draft, and the reason it is a TEST rather than a note is that the first draft was written
// by someone who had read the layering and still did it. "Work like a careful colleague", then
// instructions on restraint, verification habits and reporting style — none of it wrong advice, all
// of it somebody else's sentence to write.
//
// §25.12 layer 1 is inherited by every revision and requested by none. A deployment configuring a
// deliberately bold agent would have met this layer arguing with its own agent inside one prompt,
// and the model would have split the difference between two voices no operator chose. So layer 1
// describes the world — what exists, what it costs, how it answers — and layer 3 says who the agent
// is.
//
// The list is crude and that is the point: it catches the register, not every phrasing. A fact about
// the mechanism survives any persona ("a replacement has to match exactly one place"); a preference
// does not, and a preference stated here silently outranks the operator's.
func TestThePlatformTextPrescribesNoCharacter(t *testing.T) {
	for _, persona := range []string{
		"Work like", "work like a", // the literal opening of the first draft
		"careful", "carefully", "thoughtful", "diligent", "meticulous", "rigorous",
		"you should", "You should", "make sure", "Make sure", "be sure to",
		"stop there", "leave the rest alone", "Lead with", // the first draft's restraint and reporting rules
	} {
		if strings.Contains(platformInstructions, persona) {
			t.Errorf("the platform text contains %q: that prescribes how to BE, which is the revision author's to say — layer 1 may only describe what the environment affords", persona)
		}
	}
}

// TestThePlatformTextAvoidsEmphaticFraming pins a prompting property that is easy to lose in an
// edit. Anthropic's own migration guidance is that CRITICAL/YOU MUST framing OVERTRIGGERS on current
// models — prompts written to overcome older models' reluctance now misfire — so this text is
// written as instructions to a capable colleague. The check is deliberately narrow: it catches the
// shouted forms, not ordinary uses of "must".
func TestThePlatformTextAvoidsEmphaticFraming(t *testing.T) {
	for _, shouted := range []string{"CRITICAL", "YOU MUST", "NEVER ", "ALWAYS ", "IMPORTANT:"} {
		if strings.Contains(platformInstructions, shouted) {
			t.Errorf("the platform text contains %q; emphatic framing overtriggers on current models", shouted)
		}
	}
}
