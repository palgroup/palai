//go:build live

// The live subset runs behind the `live` build tag (credential-bound: the operator sources .env.local with
// `set -a`). It makes SINGLE-STEP real-provider calls only — the engine opens NO tool to a real provider
// (E08), so there is NO agentic benchmark here. It proves exactly two things: (1) the deterministic grader
// can SCORE a real model's single answer, and (2) a model-judge can be CALIBRATED (ranks a grounded answer
// over a hallucinated one). A model-judge is calibration only and NEVER a gate (§57.6). None of this is a
// model-quality claim — that is §6 leg 7, an E18 RC input.
package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func liveKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("live subset needs OPENAI_API_KEY (operator: set -a; . ./.env.local; set +a)")
	}
	return key
}

func liveModel() string {
	if m := os.Getenv("PALAI_EVAL_LIVE_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

func liveBaseURL() string {
	if b := os.Getenv("OPENAI_BASE_URL"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://api.openai.com/v1"
}

// chatComplete makes ONE chat/completions call — a raw net/http POST, no SDK, no tool loop (E08). The key
// rides only the Authorization header, never a log/argv.
func chatComplete(ctx context.Context, key string, messages []map[string]string) (string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"model":       liveModel(),
		"messages":    messages,
		"temperature": 0,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, liveBaseURL()+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("provider status %d: %s", resp.StatusCode, string(body))
	}
	var env struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", err
	}
	if len(env.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return env.Choices[0].Message.Content, nil
}

// TestResearchCitationSingleStepLive feeds a REAL model's single answer through the DETERMINISTIC grader:
// the harness scores a grounded, cited answer exactly as it scores a fixture. This proves the harness can
// score a real single call — NOT that the model is good.
func TestResearchCitationSingleStepLive(t *testing.T) {
	key := liveKey(t)
	ctx := context.Background()
	source := "SOURCE [S1]: The Palai control plane is written in the Go programming language."
	ans, err := chatComplete(ctx, key, []map[string]string{
		{"role": "system", "content": "Answer ONLY from the SOURCE. Every claim must cite the tag [S1]. Be brief."},
		{"role": "user", "content": source + "\nQuestion: What language is the Palai control plane written in? Cite [S1]."},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	// Grade the REAL answer deterministically, the same grader the fixtures use.
	out := Outcome{Signals: map[string]bool{
		"claims_grounded":        strings.Contains(ans, "Go"),
		"citation_offsets_valid": strings.Contains(ans, "[S1]"),
	}}
	c := EvalCase{ID: "research-live", Suite: "research", Grader: GradeInvariant, Protected: ClassProtocol,
		Input: source, Expect: map[string]bool{"claims_grounded": true, "citation_offsets_valid": true}}
	if g := GradeCase(c, out); !g.Pass {
		t.Fatalf("harness scored the real answer as ungrounded (%s): %q", g.Detail, ans)
	}
}

// TestModelJudgeCalibrationSmoke calibrates a model-judge: given a grounded answer (A) and a hallucinated
// one (B), the judge must pick A. This CALIBRATES the judge; it is never a gate (§57.6) and says nothing
// about the model's quality on the suites.
func TestModelJudgeCalibrationSmoke(t *testing.T) {
	key := liveKey(t)
	ctx := context.Background()
	pick, err := chatComplete(ctx, key, []map[string]string{
		{"role": "system", "content": "You are a faithfulness judge. Output ONLY the single letter A or B."},
		{"role": "user", "content": "SOURCE: Water boils at 100C at sea level.\nAnswer A: Water boils at 100C at sea level.\nAnswer B: Water boils at 50C at sea level.\nWhich answer is more faithful to the SOURCE? Output only A or B."},
	})
	if err != nil {
		t.Fatalf("live judge call: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(pick), "A") {
		t.Fatalf("model-judge failed calibration: expected it to prefer the grounded answer A, got %q", pick)
	}
}
