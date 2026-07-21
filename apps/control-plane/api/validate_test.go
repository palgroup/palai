package api

import (
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// TestValidateCreateRejectsBothRevisionPins proves the mutual-exclusion guard: a request may pin an
// agent revision OR a run template, never both (spec §10, §32.2) — an agent revision carries identity,
// a template is profile-free, so one request cannot mean both. Either pin alone is accepted.
func TestValidateCreateRejectsBothRevisionPins(t *testing.T) {
	agent, template := "arev_x", "rtr_y"

	both := contracts.ResponseCreateRequest{Input: "go", AgentRevisionID: &agent, RunTemplateRevisionID: &template}
	if err := validateCreate(both); err == nil {
		t.Fatal("validateCreate accepted both agent_revision_id and run_template_revision_id, want a rejection")
	}

	for name, req := range map[string]contracts.ResponseCreateRequest{
		"agent only":    {Input: "go", AgentRevisionID: &agent},
		"template only": {Input: "go", RunTemplateRevisionID: &template},
		"neither":       {Input: "go"},
	} {
		if err := validateCreate(req); err != nil {
			t.Errorf("%s: validateCreate error = %v, want nil", name, err)
		}
	}
}
