package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, reg, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestSkillManagementSurface pins the /v1/skills routes (spec §20.2, §28.15-28.16, TOL-011): a valid
// create is a 201 carrying the minted id and NO Location; install-by-URL is the same; enable is a 200; list
// is a 200 (the two absent headers are E29 T2 — neither address is mounted); an unsafe
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

	// Create a skill: 201, and NO Location. Until E29 T2 this asserted the header equalled
	// "/v1/skills/skill_1" — an address the router has never mounted, so the test was green while pinning a
	// header a client could only follow into a 404. The header is gone rather than the route added, because a
	// singular read would serve the list's three-field projection ({id, object, name}) and answer none of the
	// questions a caller has; the projection and the route belong in one later change. The direction the
	// assert gained: a written Location is a RESOLVED address, so the honest state here is no header at all.
	resp := do(t, "POST", base+"/v1/skills", `{"name":"commit-convention"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create skill status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("create skill Location = %q, want none: /v1/skills/{id} is not mounted, so any address here is one a client cannot follow", loc)
	}
	if body := readBody(t, resp); !strings.Contains(body, `"skill_1"`) {
		t.Fatalf("create skill body = %s, want the minted id — dropping the Location must not drop the only way to learn it", body)
	}

	// Install a revision by URL: 201, and no Location either — `/v1/skill-revisions/` is the exact twin of
	// the `/v1/tool-revisions/` prefix router.go describes, named in a 201 and mounted by no epic.
	resp = do(t, "POST", base+"/v1/skills/skill_1/revisions", `{"source_url":"https://example.com/skill.tgz"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install revision status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("install revision Location = %q, want none: /v1/skill-revisions/{id} is not mounted", loc)
	}
	if body := readBody(t, resp); !strings.Contains(body, `"skillrev_1"`) {
		t.Fatalf("install revision body = %s, want the revision id — enable takes it, and the body is now its only source", body)
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
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	if resp := do(t, "POST", srv.URL+"/v1/skills", `{"name":"x"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil skill registry POST status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}
