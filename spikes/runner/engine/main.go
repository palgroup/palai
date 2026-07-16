package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const engineProtocol = "engine.v1"

type frame struct {
	Protocol  string    `json:"protocol"`
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	RunID     string    `json:"run_id"`
	AttemptID string    `json:"attempt_id"`
	Sequence  uint64    `json:"sequence"`
	ReplyTo   *string   `json:"reply_to"`
	Time      time.Time `json:"time"`
	Data      any       `json:"data"`
}

func main() {
	mode := os.Getenv("PALAI_ENGINE_MODE")
	runID := os.Getenv("PALAI_RUN_ID")
	attemptID := os.Getenv("PALAI_ATTEMPT_ID")
	switch mode {
	case "valid":
		emit(runID, attemptID, map[string]any{"status": "completed"})
	case "malformed":
		fmt.Println("{not-json")
	case "oversized":
		emit(runID, attemptID, map[string]any{"padding": strings.Repeat("x", 64*1024)})
	case "stderr":
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("e", 4*1024))
		emit(runID, attemptID, map[string]any{"status": "completed"})
	case "inspect":
		emit(runID, attemptID, inspectAuthority())
	case "hang":
		time.Sleep(30 * time.Second)
	default:
		fmt.Fprintln(os.Stderr, "unsupported fixture mode")
		os.Exit(2)
	}
}

func emit(runID, attemptID string, data any) {
	value := frame{
		Protocol:  engineProtocol,
		ID:        "frame-fixture-1",
		Type:      "run.completed",
		RunID:     runID,
		AttemptID: attemptID,
		Sequence:  1,
		ReplyTo:   nil,
		Time:      time.Now().UTC(),
		Data:      data,
	}
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, "encode fixture frame")
		os.Exit(2)
	}
}

func inspectAuthority() map[string]any {
	forbiddenNames := []string{
		"OPENAI_API_KEY",
		"ANTROPHIC_API_KEY",
		"ANTHROPIC_API_KEY",
		"DATABASE_URL",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"PALAI_RUNNER_PRIVATE_KEY",
	}
	present := make([]string, 0)
	for _, name := range forbiddenNames {
		if _, exists := os.LookupEnv(name); exists {
			present = append(present, name)
		}
	}
	dockerSocketPresent := pathExists("/var/run/docker.sock") || pathExists("/run/docker.sock")
	runnerKeyPresent := pathExists("/secrets/runner.key")
	environmentNames := make([]string, 0)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	return map[string]any{
		"forbidden_environment": present,
		"environment_names":     environmentNames,
		"docker_socket_present": dockerSocketPresent,
		"runner_key_present":    runnerKeyPresent,
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
