package a2a

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAgentCardNeverLeaksInternalDetail is the crown A2A-001 guard: the Agent Card — public AND
// authenticated-extended — is a projection of PUBLISHED safe fields ONLY. It must never echo the provider
// model name, the internal tool inventory, the system prompt, or the owning tenant identity, no matter how
// the interface was published. The projection is fed a RevisionSource with distinctive sensitive markers and
// both rendered cards are asserted to contain none of them.
func TestAgentCardNeverLeaksInternalDetail(t *testing.T) {
	src := RevisionSource{
		Organization: "org_SECRETTENANT",
		Project:      "proj_SECRETTENANT",
		Model:        "provider-model-CONFIDENTIAL-x1",
		Tools:        []string{"internal_shell_TOOL", "db_admin_TOOL"},
		Instructions: "SYSTEM-PROMPT-CONFIDENTIAL do the secret thing",
		ToolSets:     []string{"toolset_CONFIDENTIAL"},
	}
	meta := PublishMeta{
		Name:         "Route Planner",
		Description:  "Plans routes.",
		Version:      "3",
		Streaming:    true,
		ExtendedCard: true,
		InputModes:   []string{"text/plain"},
		OutputModes:  []string{"application/json"},
		Skills:       []AgentSkill{{ID: "plan", Name: "Plan a route"}},
		AuthScheme:   "bearer",
	}

	iface := ProjectInterface("rev_123", src, meta)
	ep := CardEndpoint{BaseURL: "https://cp.example.com", InterfaceID: "a2aif_1"}

	forbidden := []string{
		src.Model, "internal_shell_TOOL", "db_admin_TOOL",
		src.Instructions, "toolset_CONFIDENTIAL",
		src.Organization, src.Project,
	}

	for _, name := range []string{"public", "extended"} {
		var card Card
		if name == "public" {
			card = RenderCard(iface, ep)
		} else {
			card = RenderExtendedCard(iface, ep)
		}
		blob, err := json.Marshal(card)
		if err != nil {
			t.Fatalf("marshal %s card: %v", name, err)
		}
		rendered := string(blob)
		for _, f := range forbidden {
			if strings.Contains(rendered, f) {
				t.Errorf("%s card LEAKS confidential value %q; card=%s", name, f, rendered)
			}
		}
	}

	// The card must still carry the EXACT advertised protocol version + binding + auth (A2A-001 positive half).
	card := RenderCard(iface, ep)
	if card.ProtocolVersion != "1.0" {
		t.Errorf("protocolVersion = %q, want 1.0", card.ProtocolVersion)
	}
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolBinding != "HTTP+JSON" {
		t.Errorf("supportedInterfaces = %+v, want one HTTP+JSON interface", card.SupportedInterfaces)
	}
	if card.SupportedInterfaces[0].ProtocolVersion != "1.0" {
		t.Errorf("interface protocolVersion = %q, want 1.0", card.SupportedInterfaces[0].ProtocolVersion)
	}
	if _, ok := card.SecuritySchemes["bearer"]; !ok {
		t.Errorf("card does not advertise the bearer security scheme it enforces: %+v", card.SecuritySchemes)
	}
}

// TestGovernIdentityIgnoresForgedMetadata is the crown §38.6 guard: an A2A message that forges an
// organization/project in its metadata does NOT change the tenant the run executes in — the authenticated
// bearer scope governs. This is the identity-override rejection: metadata is untrusted and cannot escalate
// across tenants.
func TestGovernIdentityIgnoresForgedMetadata(t *testing.T) {
	msg := Message{
		Role:  "user",
		Parts: []Part{{Kind: "text", Text: "hi"}},
		Metadata: map[string]any{
			"organization": "org_VICTIM",
			"project":      "proj_VICTIM",
		},
	}
	org, project := GovernIdentity("org_REAL", "proj_REAL", msg)
	if org != "org_REAL" {
		t.Errorf("forged metadata overrode organization: got %q, want org_REAL", org)
	}
	if project != "proj_REAL" {
		t.Errorf("forged metadata overrode project: got %q, want proj_REAL", project)
	}
}
