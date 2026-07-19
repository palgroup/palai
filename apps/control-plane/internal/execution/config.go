package execution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/palgroup/palai/packages/coordinator"
)

// The configuration resolution layers whose names appear in a ConfigSnapshot's provenance
// (spec §14). This slice resolves the three layers that exist today: the env-selected
// deployment default, the project baseline/policy, and the session config revision. The
// omitted layers (organization policy, pinned agent revision, run/child override) arrive
// with later phases; a value's provenance names the layer that actually set it.
const (
	layerDeployment = "deployment"
	layerProject    = "project"
	layerSession    = "session"
)

// ResolveInput is the config resolution input: the deployment default (model + credential
// ref), the project tools baseline, and the cumulative session override (spec §14). An empty
// SessionModel / nil SessionTools means the session never overrode that value, so it inherits
// the lower layer. The provider is NOT a layer here — it stays env-selected (E06 §7.3
// carve-out); only the model id and the tool set move.
type ResolveInput struct {
	DeploymentModel  string
	DeploymentSecret string // SecretRef NAME, never a credential value
	ProjectTools     []string
	SessionModel     string   // cumulative session model override ("" = never set)
	SessionTools     []string // cumulative session tools override (nil = never set)
}

// ConfigSnapshot is the resolved, redacted, content-addressed effective configuration with
// per-value provenance (spec §14). Hash is SHA-256 over the canonical JSON of the effective
// values (the LP Task 11 content_hash pattern), so identical inputs address identically.
// SecretRef stays a reference — the credential value never enters the snapshot (redaction).
type ConfigSnapshot struct {
	Hash       string            `json:"hash"`
	Model      string            `json:"model"`
	Tools      []string          `json:"tools"`
	SecretRef  string            `json:"secret_ref"`
	Provenance map[string]string `json:"provenance"`
}

// Resolve layers deployment → project → session into the effective ConfigSnapshot (spec §14).
// The session model, when set, wins over the deployment default; the session tools, when set,
// win over the project baseline. Each effective value records the layer that set it, so the
// journal and API can explain why a model/tool was selected. Pure: no I/O, so the same input
// always yields the same hash.
func Resolve(in ResolveInput) ConfigSnapshot {
	model, modelProv := in.DeploymentModel, layerDeployment
	if in.SessionModel != "" {
		model, modelProv = in.SessionModel, layerSession
	}
	// Tools baseline is the project layer; a session override replaces it wholesale (an empty
	// but non-nil session set intentionally selects no tools — spec §14.2).
	tools, toolsProv := in.ProjectTools, layerProject
	if in.SessionTools != nil {
		tools, toolsProv = in.SessionTools, layerSession
	}
	return ConfigSnapshot{
		Hash:       configContentHash(model, tools, in.DeploymentSecret),
		Model:      model,
		Tools:      tools,
		SecretRef:  in.DeploymentSecret,
		Provenance: map[string]string{"model": modelProv, "tools": toolsProv},
	}
}

// planConfigChange layers a requested change_config onto the session's cumulative override and
// the deployment/project defaults, resolves the effective ConfigSnapshot, and returns the plan
// the coordinator commits at the boundary (spec §9.3, §14). The session layer is CUMULATIVE: a
// tools-only change keeps a prior model override, and a model-only change keeps prior tools, so
// the stored revision is a full session override the per-step resolver can read directly. The
// stored row carries the session-level override (not the resolved effective value) so it stays
// a genuine override; the resolved snapshot (with provenance) rides the journal event.
func (o *Orchestrator) planConfigChange(ctx context.Context, st *attemptState, commandID string, payload []byte) (coordinator.ConfigChangePlan, error) {
	var req struct {
		Model     string   `json:"model"`
		Tools     []string `json:"tools"`
		Immediate bool     `json:"immediate"`
	}
	_ = json.Unmarshal(payload, &req)

	// Cumulative session override: start from the previous revision, then apply this change.
	sessionModel, sessionTools := "", []string(nil)
	if prev, found, err := o.spine.LatestSessionConfig(ctx, st.tenant, st.sessionID); err != nil {
		return coordinator.ConfigChangePlan{}, err
	} else if found {
		sessionModel, sessionTools = prev.Model, prev.Tools
	}
	if req.Model != "" {
		sessionModel = req.Model
	}
	if req.Tools != nil {
		sessionTools = req.Tools
	}

	// The project baseline tools layer (spec §14.4). A project with no policy has none.
	policy, err := o.spine.ProjectConfig(ctx, st.tenant)
	if err != nil {
		return coordinator.ConfigChangePlan{}, err
	}

	snap := Resolve(ResolveInput{
		DeploymentModel:  o.route.Model,
		DeploymentSecret: string(o.route.Secret),
		ProjectTools:     policy.DefaultTools,
		SessionModel:     sessionModel,
		SessionTools:     sessionTools,
	})

	// The row stores the session-level override; nil tools stay NULL (untouched), not [].
	var toolsJSON []byte
	if sessionTools != nil {
		toolsJSON, _ = json.Marshal(sessionTools)
	}
	revised, _ := json.Marshal(map[string]any{
		"command_id": commandID,
		"immediate":  req.Immediate,
		"snapshot":   snap, // redacted: secret ref only, never a value
	})
	return coordinator.ConfigChangePlan{
		RevisionID:     newConfigRevisionID(),
		Model:          sessionModel,
		ToolsJSON:      toolsJSON,
		SnapshotHash:   snap.Hash,
		Immediate:      req.Immediate,
		RevisedPayload: revised,
	}, nil
}

// newConfigRevisionID mints an opaque config revision id.
func newConfigRevisionID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return "cfgrev_" + hex.EncodeToString(raw[:])
}

// configContentHash is the canonical content address of a snapshot's effective values. It
// hashes only the effective config (model, tools, secret ref), never the provenance, so the
// address is stable across equivalent resolutions from different layers.
func configContentHash(model string, tools []string, secretRef string) string {
	if tools == nil {
		tools = []string{}
	}
	canonical, _ := json.Marshal(struct {
		Model     string   `json:"model"`
		Tools     []string `json:"tools"`
		SecretRef string   `json:"secret_ref"`
	}{model, tools, secretRef})
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}
