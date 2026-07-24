package a2a

import "strings"

// RevisionSource is the SENSITIVE input to the card projection: a published AgentRevision's internal
// executable config. Every field here is confidential — the provider model name, the internal tool
// inventory, the system prompt, and the owning tenant identity. NONE of it may appear on an Agent Card
// (A2A-001). It exists as the projection's INPUT so the no-leak guarantee is a property of ProjectInterface,
// tested against the real sensitive values rather than asserted vacuously.
type RevisionSource struct {
	Organization string
	Project      string
	Model        string   // provider model name — MUST NOT leak
	Tools        []string // internal tool inventory — MUST NOT leak
	Instructions string   // system prompt — MUST NOT leak
	ToolSets     []string // MUST NOT leak
}

// PublishMeta is the publisher-curated SAFE card metadata: what the interface owner explicitly chose to
// advertise. These fields are the only ones that reach a card. Skills here are published, owner-authored
// capability descriptors — NOT the internal Tools inventory.
type PublishMeta struct {
	Name              string
	Description       string
	Version           string
	Streaming         bool
	PushNotifications bool
	ExtendedCard      bool
	InputModes        []string
	OutputModes       []string
	Skills            []AgentSkill
	AuthScheme        string // advertised security scheme name, e.g. "bearer"
}

// AgentSkill is a published, owner-authored skill descriptor (A2A AgentCard.skills). It is NOT derived from
// the internal tool inventory — a publisher writes it, so it carries no confidential detail by construction.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
	// ExtendedOnly skills appear ONLY on the authenticated extended card, never the public card.
	ExtendedOnly bool `json:"-"`
}

// PublishedInterface is the SAFE stored projection (one a2a_interfaces row). It carries only fields that are
// safe to serve publicly; it structurally cannot hold the RevisionSource internals. The AgentRevisionID pin
// is stored for provenance but is NEVER rendered onto a card.
type PublishedInterface struct {
	ID                string
	Organization      string
	Project           string
	Name              string
	Description       string
	Version           string
	AgentProfileID    string
	AgentRevisionID   string
	Streaming         bool
	PushNotifications bool
	ExtendedCard      bool
	InputModes        []string
	OutputModes       []string
	Skills            []AgentSkill
	AuthScheme        string
	ETag              string
}

// ProjectInterface is the SINGLE card-projection boundary (A2A-001): it takes the sensitive RevisionSource
// and the publisher's safe PublishMeta and returns a PublishedInterface carrying ONLY safe fields. The
// RevisionSource is read for the provenance pin (revisionID) and NOTHING else — its Model/Tools/Instructions
// never flow into the output. A crown RED-first test publishes an interface from a revision with distinctive
// sensitive values and asserts neither card echoes them.
//
// ponytail: LEAKY on purpose in this first commit (RED). The description appends the model name and the
// internal tools become skills — the no-leak test must catch it before the fix lands.
func ProjectInterface(revisionID string, src RevisionSource, meta PublishMeta) PublishedInterface {
	skills := meta.Skills
	for _, t := range src.Tools {
		skills = append(skills, AgentSkill{ID: t, Name: t})
	}
	return PublishedInterface{
		Name:              meta.Name,
		Description:       strings.TrimSpace(meta.Description + " (model: " + src.Model + ")"),
		Version:           meta.Version,
		Organization:      src.Organization,
		Project:           src.Project,
		AgentRevisionID:   revisionID,
		Streaming:         meta.Streaming,
		PushNotifications: meta.PushNotifications,
		ExtendedCard:      meta.ExtendedCard,
		InputModes:        meta.InputModes,
		OutputModes:       meta.OutputModes,
		Skills:            skills,
		AuthScheme:        meta.AuthScheme,
	}
}

// Card is the rendered A2A 1.0 Agent Card (the JSON discovery document). Every field is a safe projection.
type Card struct {
	Name              string             `json:"name"`
	Description       string             `json:"description,omitempty"`
	Version           string             `json:"version"`
	ProtocolVersion   string             `json:"protocolVersion"`
	SupportedInterfaces []AgentInterface `json:"supportedInterfaces"`
	PreferredTransport string            `json:"preferredTransport"`
	Capabilities      AgentCapabilities  `json:"capabilities"`
	DefaultInputModes  []string          `json:"defaultInputModes,omitempty"`
	DefaultOutputModes []string          `json:"defaultOutputModes,omitempty"`
	Skills            []AgentSkill       `json:"skills,omitempty"`
	SecuritySchemes   map[string]any     `json:"securitySchemes,omitempty"`
	Security          []map[string][]string `json:"security,omitempty"`
	SupportsAuthenticatedExtendedCard bool `json:"supportsAuthenticatedExtendedCard"`
}

// AgentInterface is one advertised transport interface (exact url + binding + version, A2A-001).
type AgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

// AgentCapabilities is the advertised capability set. It reflects what THIS server serves for the interface.
type AgentCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
	ExtendedAgentCard bool `json:"extendedAgentCard"`
}

// CardEndpoint locates an interface's HTTP+JSON base so the card advertises the exact URL clients call.
type CardEndpoint struct {
	BaseURL     string // e.g. "https://cp.example.com"
	InterfaceID string
}

// interfaceURL is the HTTP+JSON binding base path for an interface.
func (e CardEndpoint) interfaceURL() string {
	return strings.TrimRight(e.BaseURL, "/") + "/v1/a2a/interfaces/" + e.InterfaceID
}

// RenderCard builds the PUBLIC (unauthenticated) Agent Card: name/version/capabilities/interfaces/auth plus
// public skills. It carries NO sensitive detail and NO ExtendedOnly skill.
func RenderCard(iface PublishedInterface, ep CardEndpoint) Card {
	return renderCard(iface, ep, false)
}

// RenderExtendedCard builds the AUTHENTICATED extended Agent Card: the public card plus ExtendedOnly skills
// and richer detail. It still carries no RevisionSource internals — "extended" means more PUBLISHED detail,
// never the model/tool/tenant internals.
func RenderExtendedCard(iface PublishedInterface, ep CardEndpoint) Card {
	return renderCard(iface, ep, true)
}

func renderCard(iface PublishedInterface, ep CardEndpoint, extended bool) Card {
	var skills []AgentSkill
	for _, s := range iface.Skills {
		if s.ExtendedOnly && !extended {
			continue
		}
		skills = append(skills, s)
	}
	c := Card{
		Name:            iface.Name,
		Description:     iface.Description,
		Version:         iface.Version,
		ProtocolVersion: ProtocolVersion,
		SupportedInterfaces: []AgentInterface{{
			URL:             ep.interfaceURL(),
			ProtocolBinding: HTTPJSONBinding,
			ProtocolVersion: ProtocolVersion,
		}},
		PreferredTransport: HTTPJSONBinding,
		Capabilities: AgentCapabilities{
			Streaming:         iface.Streaming,
			PushNotifications: iface.PushNotifications,
			ExtendedAgentCard: iface.ExtendedCard,
		},
		DefaultInputModes:                 iface.InputModes,
		DefaultOutputModes:                iface.OutputModes,
		Skills:                            skills,
		SupportsAuthenticatedExtendedCard: iface.ExtendedCard,
	}
	if scheme := strings.TrimSpace(iface.AuthScheme); scheme != "" {
		// Advertise the EXACT auth the server enforces (A2A-001): a bearer HTTP scheme. No secret material.
		c.SecuritySchemes = map[string]any{scheme: map[string]any{
			"httpAuthSecurityScheme": map[string]any{"scheme": "bearer"},
		}}
		c.Security = []map[string][]string{{scheme: {}}}
	}
	return c
}
