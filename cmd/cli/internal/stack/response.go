package stack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// CreateResponse admits a response over the bootstrap API key — the documented command
// path with no manual SQL (LP-001). It prints the created envelope (id + status) as JSON
// on stdout. LP-0 does not drive execution (that is Task 15), so the response is admitted
// and retrievable but not yet terminal.
func CreateResponse(input string) error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return fmt.Errorf("read api key: %w", err)
	}
	body, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, cfg.BaseURL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", "cli-"+randomHex(12))
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("POST /v1/responses: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("response create failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	fmt.Fprintln(os.Stdout, strings.TrimSpace(string(raw)))
	return nil
}
