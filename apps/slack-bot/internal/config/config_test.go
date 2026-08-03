package config

import (
	"strings"
	"testing"
)

func TestConfigRefusesAMissingBotID(t *testing.T) {
	t.Setenv("PALAI_BOT_ID", "")
	t.Setenv("PALAI_API_URL", "https://cp.example")
	t.Setenv("PALAI_API_KEY", "ak_1")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted an empty PALAI_BOT_ID; a bot with no registry row has nothing to be")
	}
}

func TestConfigNamesTheVariableThatIsWrong(t *testing.T) {
	t.Setenv("PALAI_BOT_ID", "bot_1")
	t.Setenv("PALAI_API_URL", "")
	t.Setenv("PALAI_API_KEY", "ak_1")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PALAI_API_URL") {
		t.Fatalf("error %v does not name the variable that is wrong", err)
	}
}

// TestConfigRefusesAMissingAPIKey is TestConfigNamesTheVariableThatIsWrong's sibling for the third
// variable Load checks.
func TestConfigRefusesAMissingAPIKey(t *testing.T) {
	t.Setenv("PALAI_BOT_ID", "bot_1")
	t.Setenv("PALAI_API_URL", "https://cp.example")
	t.Setenv("PALAI_API_KEY", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PALAI_API_KEY") {
		t.Fatalf("error %v does not name the variable that is wrong", err)
	}
}

// TestConfigRefusesAMissingDatabaseURL is the fourth variable's sibling. It sets the first three so
// PALAI_BOT_DATABASE_URL is the one variable this test leaves unset.
func TestConfigRefusesAMissingDatabaseURL(t *testing.T) {
	t.Setenv("PALAI_BOT_ID", "bot_1")
	t.Setenv("PALAI_API_URL", "https://cp.example")
	t.Setenv("PALAI_API_KEY", "ak_1")
	t.Setenv("PALAI_BOT_DATABASE_URL", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PALAI_BOT_DATABASE_URL") {
		t.Fatalf("error %v does not name the variable that is wrong", err)
	}
}

// TestConfigLoadsAllFourVariables is the happy path: all four variables set, all four fields
// carried through unchanged.
func TestConfigLoadsAllFourVariables(t *testing.T) {
	t.Setenv("PALAI_BOT_ID", "bot_7bc2d6980ff13382fa95dc67eb531a65")
	t.Setenv("PALAI_API_URL", "http://127.0.0.1:60351")
	t.Setenv("PALAI_API_KEY", "ak_1")
	t.Setenv("PALAI_BOT_DATABASE_URL", "postgres://localhost/slack_bot")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	want := Config{
		BotID:        "bot_7bc2d6980ff13382fa95dc67eb531a65",
		PalaiBaseURL: "http://127.0.0.1:60351",
		PalaiAPIKey:  "ak_1",
		DatabaseURL:  "postgres://localhost/slack_bot",
	}
	if cfg != want {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}
