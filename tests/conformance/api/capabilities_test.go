package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCapabilities proves the discovery surface GET /v1/capabilities: it requires a bearer
// key, publishes the LP-0 maturity/isolation posture and capability matrix, and reflects
// the configured store:false retention TTL (spec §20.9 configured retention through
// discovery; the value `palai local doctor` reads for its retention_ttl check).
func TestCapabilities(t *testing.T) {
	srv := newTestServer(t)

	t.Run("requires a bearer key", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/capabilities", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated capabilities = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("publishes posture and disabled retention by default", func(t *testing.T) {
		body := getCapabilities(t, srv)
		if body.Object != "capabilities" {
			t.Errorf("object = %q, want capabilities", body.Object)
		}
		if body.Maturity != "preview" {
			t.Errorf("maturity = %q, want preview", body.Maturity)
		}
		if body.Isolation != "development" {
			t.Errorf("isolation = %q, want development", body.Isolation)
		}
		if body.Capabilities["responses"] != "preview" {
			t.Errorf("responses capability = %q, want preview", body.Capabilities["responses"])
		}
		if body.Capabilities["sessions"] != "unavailable" || body.Capabilities["workspaces"] != "unavailable" {
			t.Errorf("sessions/workspaces should be unavailable: %+v", body.Capabilities)
		}
		if body.Retention.StoreFalseTTLSeconds != 0 {
			t.Errorf("default store_false_ttl_seconds = %d, want 0 (disabled)", body.Retention.StoreFalseTTLSeconds)
		}
	})

	t.Run("reflects the configured retention TTL", func(t *testing.T) {
		t.Setenv("PALAI_RETENTION_STORE_FALSE_TTL", "1h")
		body := getCapabilities(t, srv)
		if body.Retention.StoreFalseTTLSeconds != 3600 {
			t.Fatalf("store_false_ttl_seconds = %d, want 3600", body.Retention.StoreFalseTTLSeconds)
		}
	})
}

type capabilitiesResponse struct {
	Object    string `json:"object"`
	Maturity  string `json:"maturity"`
	Isolation string `json:"isolation"`
	Retention struct {
		StoreFalseTTLSeconds int `json:"store_false_ttl_seconds"`
	} `json:"retention"`
	Capabilities map[string]string `json:"capabilities"`
}

// getCapabilities fetches the discovery body with a valid bearer key and decodes it.
func getCapabilities(t *testing.T, srv *httptest.Server) capabilitiesResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/capabilities", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities = %d, want 200", resp.StatusCode)
	}
	var body capabilitiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	return body
}
