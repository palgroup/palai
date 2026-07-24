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

// GetResponse retrieves a response by id and prints its NORMALIZED projection —
// {"id","output_text","status"} as canonical JSON — on stdout. It is the palai CLI's leg of the
// E16 T8 four-client parity journey: the SAME shared run, retrieved and normalized by all four
// clients, is asserted byte-identical. Base URL + API key resolve from PALAI_BASE_URL/PALAI_API_KEY
// (the journey sets these) or fall back to the .palai config. A 410 tombstone (a purged store:false
// response) is reported honestly: it prints {"status":410,...} and returns an error so the caller can
// assert the typed-gone surface. The output text is the concatenation of the response output items'
// text fields — the same extraction every client applies to the same retrieved bytes.
func GetResponse(id string) error {
	baseURL, key := os.Getenv("PALAI_BASE_URL"), os.Getenv("PALAI_API_KEY")
	if baseURL == "" || key == "" {
		cfg, p, err := loadConfig()
		if err != nil {
			return err
		}
		if baseURL == "" {
			baseURL = cfg.BaseURL
		}
		if key == "" {
			key, err = readTrimmed(p.apiKey)
			if err != nil {
				return fmt.Errorf("read api key: %w", err)
			}
		}
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/responses/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("GET /v1/responses/%s: %w", id, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusGone {
		fmt.Fprintln(os.Stdout, `{"gone":true,"status":410}`)
		return fmt.Errorf("response %s is gone (410 tombstone)", id)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("response retrieve failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var r struct {
		ID     string           `json:"id"`
		Status string           `json:"status"`
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("decode response %s: %w", id, err)
	}
	out, err := json.Marshal(map[string]any{"id": r.ID, "output_text": outputText(r.Output), "status": r.Status})
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, string(out))
	return nil
}

// outputText concatenates the text fields of a response's output items — the canonical, client-agnostic
// extraction the four parity clients all apply to the same retrieved bytes.
func outputText(items []map[string]any) string {
	var b strings.Builder
	for _, it := range items {
		if t, ok := it["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}
