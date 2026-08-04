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
