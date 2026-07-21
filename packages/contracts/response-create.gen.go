// Code generated from the canonical ResponseCreateRequest schema; DO NOT EDIT.
package contracts

type ResponseCreateRequest struct {
	AgentRevisionID       *string          `json:"agent_revision_id,omitempty"`
	Background            bool             `json:"background,omitempty"`
	Budget                map[string]any   `json:"budget,omitempty"`
	Callback              map[string]any   `json:"callback,omitempty"`
	Capabilities          []string         `json:"capabilities,omitempty"`
	Context               map[string]any   `json:"context,omitempty"`
	Delegation            map[string]any   `json:"delegation,omitempty"`
	Engine                *string          `json:"engine,omitempty"`
	Input                 any              `json:"input"`
	Instructions          string           `json:"instructions,omitempty"`
	MaxOutputTokens       int              `json:"max_output_tokens,omitempty"`
	MaxToolCalls          int              `json:"max_tool_calls,omitempty"`
	Metadata              map[string]any   `json:"metadata,omitempty"`
	Model                 string           `json:"model,omitempty"`
	Output                map[string]any   `json:"output,omitempty"`
	ParallelToolCalls     bool             `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID    *string          `json:"previous_response_id,omitempty"`
	Repository            map[string]any   `json:"repository,omitempty"`
	RunTemplateRevisionID *string          `json:"run_template_revision_id,omitempty"`
	SessionID             *string          `json:"session_id,omitempty"`
	Skills                []string         `json:"skills,omitempty"`
	Store                 bool             `json:"store,omitempty"`
	Stream                bool             `json:"stream,omitempty"`
	ToolChoice            string           `json:"tool_choice,omitempty"`
	ToolSets              []string         `json:"tool_sets,omitempty"`
	Tools                 []map[string]any `json:"tools,omitempty"`
	Workspace             map[string]any   `json:"workspace,omitempty"`
}
