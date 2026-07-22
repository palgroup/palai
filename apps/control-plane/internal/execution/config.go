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
// (spec §14). Ordered low → high: the env-selected deployment default, the project's DB-backed model
// route (E13 Task 8), the project baseline/policy, the pinned agent revision / run-template revision
// (E11 Task 1), and the session config revision / run override on top. The remaining omitted layers (organization
// policy, child override) arrive with later phases; a value's provenance names the layer that
// actually set it. layerAgentRevision covers both an AgentRevision and a RunTemplateRevision —
// spec §14 resolves both from ONE logical ExecutionSpec, and a template is a profile-free pinned
// revision, so they share this layer.
const (
	layerDeployment    = "deployment"
	layerProjectRoute  = "project_route"
	layerProject       = "project"
	layerAgentRevision = "agent_revision"
	layerSession       = "session"
)

// ResolveInput is the config resolution input: the deployment default (model + credential
// ref), the project's model route, the project tools baseline, and the cumulative session override
// (spec §14). An empty SessionModel / nil SessionTools means the session never overrode that value,
// so it inherits the lower layer. The PROVIDER is not a layer here: which adapter a call goes to is
// decided by the route itself (execution.effectiveRoute), not by config resolution — this input
// resolves the model id, its credential ref, and the tool set.
type ResolveInput struct {
	DeploymentModel  string
	DeploymentSecret string // SecretRef NAME, never a credential value
	// ProjectRouteModel / ProjectRouteSecret are the project's DB-backed model route (E13 Task 8), the
	// layer directly above the deployment default: the model id its published revision selects and the
	// tenant-qualified credential REF of the connection that revision binds. Empty means the project
	// published no route, leaving resolution bit-identical to the pre-T8 deployment-only path.
	ProjectRouteModel  string
	ProjectRouteSecret string
	ProjectTools       []string
	// AgentRevisionID / Model / Tools are the pinned AgentRevision or RunTemplateRevision layer
	// (E11 Task 1, AGT-001). An empty AgentRevisionID means the run pins no revision — the layer is
	// skipped and resolution behaves exactly as before (the profile-free path). AgentRevisionModel,
	// when set, pins the model above the deployment default (a session override still wins).
	// AgentRevisionTools, when non-nil, is a capability CEILING intersected with the resolved tools:
	// a tool the revision does not declare never reaches the effective set (63.4 "capability never
	// expands"), even when the project baseline or a session override carries it.
	AgentRevisionID    string
	AgentRevisionModel string
	AgentRevisionTools []string
	// AgentRevisionToolSetTools is the model-visible short names contributed by the pinned revision's
	// tool_sets (E12 Task 2, EXT-003). They UNION into the resolved baseline (provenance agent_revision)
	// BEFORE the AgentRevisionTools ceiling intersects — so a revision can both grant a registered tool
	// via a set AND narrow the whole set with a ceiling. Empty leaves resolution bit-identical to before.
	AgentRevisionToolSetTools []string
	SessionModel              string   // cumulative session model override ("" = never set)
	SessionTools              []string // cumulative session tools override (nil = never set)
	// SkillPinsJSON is the run's frozen skill set (E12 Task 7, spec §28.16): the JSON-encoded
	// []{name,description,digest,path} pinned at run-start. nil/empty means the run resolved no skills —
	// the snapshot then carries no Skills and the hash is BIT-IDENTICAL to the pre-skills path (a
	// skill-less run's config address never moves, the T1 advertising regression discipline). A non-empty
	// pin folds into both the Skills rider and the content hash, so a mid-run enable that would change the
	// pin (it cannot — the pin is frozen) would change the hash, keeping the checkpoint coherent.
	SkillPinsJSON []byte
}

// SkillRef is one entry of a run's progressively-loaded skill set (spec §28.16): the model-visible name
// + description (the context rider), the exact pinned digest, and the workspace-relative body path the
// file tool reads on-demand. It grants NO capability — metadata + a file location, never a tool.
type SkillRef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Digest      string `json:"digest"`
	Path        string `json:"path"`
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
	// Skills is the run's frozen skill set (E12 Task 7, spec §28.16), empty for a skill-less run. It rides
	// the snapshot as a progressive-loading rider (metadata only — the body reads from Path via the file
	// tool), and folds into Hash only when non-empty (bit-compat for a skill-less run).
	Skills []SkillRef `json:"skills,omitempty"`
}

// Resolve layers deployment → project → session into the effective ConfigSnapshot (spec §14).
// The session model, when set, wins over the deployment default; the session tools, when set,
// win over the project baseline. Each effective value records the layer that set it, so the
// journal and API can explain why a model/tool was selected. Pure: no I/O, so the same input
// always yields the same hash.
func Resolve(in ResolveInput) ConfigSnapshot {
	// Model: deployment default < project route < pinned revision < session/run override.
	model, modelProv := in.DeploymentModel, layerDeployment
	// The credential ref follows the model's own two lowest layers: a project routed onto its own
	// connection redeems THAT connection, never the deployment credential.
	secretRef := in.DeploymentSecret
	if in.ProjectRouteModel != "" {
		model, modelProv = in.ProjectRouteModel, layerProjectRoute
	}
	if in.ProjectRouteSecret != "" {
		secretRef = in.ProjectRouteSecret
	}
	if in.AgentRevisionModel != "" {
		model, modelProv = in.AgentRevisionModel, layerAgentRevision
	}
	if in.SessionModel != "" {
		model, modelProv = in.SessionModel, layerSession
	}
	// Tools baseline is the project layer; a session override replaces it wholesale (an empty
	// but non-nil session set intentionally selects no tools — spec §14.2).
	tools, toolsProv := in.ProjectTools, layerProject
	if in.SessionTools != nil {
		tools, toolsProv = in.SessionTools, layerSession
	}
	// The pinned revision's tool_sets GRANT registered tools: union their short names onto the baseline
	// (E12 Task 2). The union carries the agent_revision provenance, so a set-granted tool is attributed
	// to the revision that pinned it.
	if len(in.AgentRevisionToolSetTools) > 0 {
		tools, toolsProv = unionTools(tools, in.AgentRevisionToolSetTools), layerAgentRevision
	}
	// The pinned revision's tool set is a capability CEILING: intersect it LAST, so neither the
	// project baseline, a session override, nor a set grant can select a tool the revision does not
	// declare (spec §10 capability restriction, 63.4). The revision then owns the effective set's provenance.
	if in.AgentRevisionID != "" && in.AgentRevisionTools != nil {
		tools, toolsProv = intersectTools(tools, in.AgentRevisionTools), layerAgentRevision
	}
	provenance := map[string]string{"model": modelProv, "tools": toolsProv}
	if in.AgentRevisionID != "" {
		// The pinned revision id rides the provenance (never the content hash): AGT-001's "run's
		// snapshot names the revision it ran under". The hash addresses only effective values, so an
		// equivalent config from a different revision still content-addresses identically.
		provenance["agent_revision"] = in.AgentRevisionID
	}
	// Decode the run's frozen skill pins (E12 Task 7). An empty pin leaves skills nil, so the snapshot
	// and hash are bit-identical to the pre-skills path; a non-empty pin rides the snapshot and folds into
	// the hash. A malformed pin decodes to nil — a run never fails to resolve on its own frozen JSON.
	var skills []SkillRef
	if len(in.SkillPinsJSON) > 0 {
		_ = json.Unmarshal(in.SkillPinsJSON, &skills)
	}
	if in.AgentRevisionID != "" && len(skills) > 0 {
		provenance["skills"] = in.AgentRevisionID
	}
	return ConfigSnapshot{
		Hash:       configContentHash(model, tools, secretRef, skills),
		Model:      model,
		Tools:      tools,
		SecretRef:  secretRef,
		Provenance: provenance,
		Skills:     skills,
	}
}

// unionTools appends the grant's tools not already present, preserving order (baseline first, then the
// new grants in their given order). A tool already in the baseline is not duplicated.
func unionTools(baseline, grants []string) []string {
	seen := make(map[string]struct{}, len(baseline))
	for _, t := range baseline {
		seen[t] = struct{}{}
	}
	out := append([]string(nil), baseline...)
	for _, t := range grants {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

// intersectTools returns the resolved tools that the ceiling also permits, preserving the resolved
// order (the effective registry is a subset of what was resolved, never a reordering). The ceiling
// is a membership filter, so a resolved tool absent from it is dropped.
func intersectTools(resolved, ceiling []string) []string {
	allowed := make(map[string]struct{}, len(ceiling))
	for _, t := range ceiling {
		allowed[t] = struct{}{}
	}
	out := make([]string, 0, len(resolved))
	for _, t := range resolved {
		if _, ok := allowed[t]; ok {
			out = append(out, t)
		}
	}
	return out
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

	// The run's pinned agent/template revision (spec §14, AGT-001) — the SAME layer effectiveConfigHash
	// and effectiveModel resolve through, so a config.revised snapshot and a checkpoint never record
	// divergent config for the same state (the checkpoint.go:185-186 promise). Without this a pinned
	// run's config.revised would drop the ceiling and the provenance and diverge from the checkpoint hash.
	revID, revModel, revTools, revToolSetTools, skillPins, err := o.spine.PinnedExecConfig(ctx, st.tenant, string(st.attempt.RunID))
	if err != nil {
		return coordinator.ConfigChangePlan{}, err
	}

	// The project's DB-backed route is the same layer dispatch routes through (E13 T8), so a config
	// revision and a checkpoint can never disagree about which model + credential the run is on.
	route, err := o.effectiveRoute(ctx, st)
	if err != nil {
		return coordinator.ConfigChangePlan{}, err
	}
	in := ResolveInput{
		ProjectTools:              policy.DefaultTools,
		AgentRevisionID:           revID,
		AgentRevisionModel:        revModel,
		AgentRevisionTools:        revTools,
		AgentRevisionToolSetTools: revToolSetTools,
		SkillPinsJSON:             skillPins,
		SessionModel:              sessionModel,
		SessionTools:              sessionTools,
	}
	o.routeLayers(route, &in)
	snap := Resolve(in)

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
func configContentHash(model string, tools []string, secretRef string, skills []SkillRef) string {
	if tools == nil {
		tools = []string{}
	}
	canonical, _ := json.Marshal(struct {
		Model     string   `json:"model"`
		Tools     []string `json:"tools"`
		SecretRef string   `json:"secret_ref"`
		// Skills is omitted entirely when empty (omitempty), so a skill-less run hashes over EXACTLY the
		// pre-skills fields — the address never moves for a run that configures no skills (T1 regression).
		Skills []SkillRef `json:"skills,omitempty"`
	}{model, tools, secretRef, skills})
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}
