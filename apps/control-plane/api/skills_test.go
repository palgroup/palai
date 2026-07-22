package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeSkillRegistry scripts each seam outcome so the handler contract is exercised without a database.
type fakeSkillRegistry struct {
	createSkill SkillResult
	install     SkillResult
	enable      SkillResult
	list        SkillResult
	lastBody    []byte
}

func (f *fakeSkillRegistry) CreateSkill(_ context.Context, _ middleware.Scope, body []byte) (SkillResult, error) {
	f.lastBody = body
	return f.createSkill, nil
}
func (f *fakeSkillRegistry) InstallSkillRevision(_ context.Context, _ middleware.Scope, _ string, body []byte) (SkillResult, error) {
	f.lastBody = body
	return f.install, nil
}
func (f *fakeSkillRegistry) EnableSkillRevision(context.Context, middleware.Scope, string, string) (SkillResult, error) {
	return f.enable, nil
}
func (f *fakeSkillRegistry) ListSkills(context.Context, middleware.Scope) (SkillResult, error) {
	return f.list, nil
}

func skillTestServer(t *testing.T, reg *fakeSkillRegistry) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, reg, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestSkillManagementSurface pins the /v1/skills routes (spec §20.2, §28.15-28.16, TOL-011): a valid
// create is a 201 with a Location; install-by-URL is a 201; enable is a 200; list is a 200; an unsafe
// archive / denied source is a 400; a name collision or a scan-findings enable is a 409; an unknown
// skill/revision is a 404.
func TestSkillManagementSurface(t *testing.T) {
	reg := &fakeSkillRegistry{
		createSkill: SkillResult{Body: []byte(`{"id":"skill_1","object":"skill"}`)},
		install:     SkillResult{Body: []byte(`{"id":"skillrev_1","object":"skill_revision"}`)},
		enable:      SkillResult{Body: []byte(`{"id":"skillrev_1","state":"enabled"}`)},
		list:        SkillResult{Body: []byte(`{"object":"list","data":[]}`)},
	}
	base := skillTestServer(t, reg)

	// Create a skill: 201 with a Location pointing at the minted id.
	resp := do(t, "POST", base+"/v1/skills", `{"name":"commit-convention"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create skill status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/skills/skill_1" {
		t.Fatalf("create skill Location = %q, want /v1/skills/skill_1", loc)
	}

	// Install a revision by URL: 201.
	if resp := do(t, "POST", base+"/v1/skills/skill_1/revisions", `{"source_url":"https://example.com/skill.tgz"}`, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("install revision status = %d, want 201", resp.StatusCode)
	}

	// Enable a revision: 200.
	if resp := do(t, "POST", base+"/v1/skills/skill_1/revisions/skillrev_1/enable", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("enable revision status = %d, want 200", resp.StatusCode)
	}

	// List: 200.
	if resp := do(t, "GET", base+"/v1/skills", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("list skills status = %d, want 200", resp.StatusCode)
	}

	// An unsafe archive / denied source is a 400.
	reg.install = SkillResult{BadField: true}
	if resp := do(t, "POST", base+"/v1/skills/skill_1/revisions", `{"source_url":"http://169.254.169.254/"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsafe-source status = %d, want 400", resp.StatusCode)
	}

	// A scan-findings enable is a 409 (the revision is stuck at quarantined).
	reg.enable = SkillResult{Conflict: true}
	if resp := do(t, "POST", base+"/v1/skills/skill_1/revisions/skillrev_2/enable", ``, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("scan-findings enable status = %d, want 409", resp.StatusCode)
	}

	// An unknown revision is a 404.
	reg.enable = SkillResult{NotFound: true}
	if resp := do(t, "POST", base+"/v1/skills/skill_1/revisions/skillrev_missing/enable", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-revision status = %d, want 404", resp.StatusCode)
	}
}

// TestSkillRoutesUnmountedWhenNil proves the nil-seam guard: a tier that passes no skill registry never
// mounts the routes, so a POST is a 404 (the tools/agents nil-guard precedent).
func TestSkillRoutesUnmountedWhenNil(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	if resp := do(t, "POST", srv.URL+"/v1/skills", `{"name":"x"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil skill registry POST status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}
