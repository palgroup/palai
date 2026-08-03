//go:build component

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/storage"
)

// TestDesiredConfigJourneyWritesADocumentTheDeploymentReadReports is the E29 desired-configuration journey:
// the SHIPPED router with only this seam mounted, over a REAL Postgres.
//
// IT EXISTS ON TOP OF THE UNIT TESTS BECAUSE THOSE CANNOT PROVE THE ROUTE. api's own tests prove the
// allow-list, the value grammar and the drift rule; none of them writes a row, and a field can be validated,
// stored, and then dropped by the projection — which looks like success from either side alone. And two of
// the four properties below are DATABASE guarantees that no amount of Go can assert:
//
//  1. The write reaches the durable spine and the SAME read surface reports it, against this process's
//     real environment.
//  2. A refused setting never lands. The refusal is a 400 AND the table is unchanged.
//  3. The journal is APPEND-ONLY, enforced by 000052's self-re-asserting REVOKE rather than by convention:
//     the runtime role is denied UPDATE and DELETE by Postgres.
//  4. "The current document" is the highest REVISION UNDER ONE SCOPE, and the read says so with an explicit
//     scope predicate and ORDER BY. An unordered LIMIT 1 has decided a security outcome in this tree twice;
//     here the row it returns is the one a bring-up turns into the process's environment, so an arbitrary
//     row is an arbitrary machine — and without the scope predicate a pool's document written later would
//     be handed to the CONTROL PLANE's bring-up, which is worse than none because it would look like it
//     worked.
func TestDesiredConfigJourneyWritesADocumentTheDeploymentReadReports(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// THE PROCESS'S OWN ENVIRONMENT, set here so the drift assertion below is about a value this test
	// controls rather than about whatever the tier's container happened to be started with.
	t.Setenv("PALAI_DISPATCH_WORKERS", "1")

	router := api.NewRouter(provisionVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithDesiredConfig(repo))
	ts := httptest.NewServer(router)
	defer ts.Close()

	// --- 1. THE WRITE, AND THE READ THAT REPORTS IT ---------------------------------------------------
	put(t, ts.URL, `{"settings":{"PALAI_DISPATCH_WORKERS":"6","PALAI_QUEUE_DEADLINE":"9m"}}`, http.StatusOK)
	body := deploymentRead(t, ts.URL)
	if body.Desired == nil {
		t.Fatal("GET /v1/deployment reports no desired document after a successful write. The panel would " +
			"report a save that nothing consults — worse than the dotenv file it replaces, because the file worked")
	}
	if body.Desired.Revision != 1 {
		t.Errorf("first revision = %d, want 1", body.Desired.Revision)
	}
	// THE SCOPE IS ON THE WIRE. A screen that could not tell which process a document configures would
	// render the control plane's settings under a heading a reader takes as "this machine" — the misreading
	// the plane was put in the key to prevent, and one a body without these two fields makes unavoidable.
	if body.Desired.Plane != "control_plane" || body.Desired.Scope != "" {
		t.Errorf("desired scope = %q/%q, want the control-plane singleton", body.Desired.Plane, body.Desired.Scope)
	}
	if !body.Desired.Pending {
		t.Errorf("desired asks for PALAI_DISPATCH_WORKERS=6 against a process holding 1, and the surface reports "+
			"no pending bring-up. drifted = %v", body.Desired.Drifted)
	}
	row := settingRow(t, body, "PALAI_DISPATCH_WORKERS")
	if row.Desired != "6" || !row.DesiredSet || !row.Drift {
		t.Errorf("PALAI_DISPATCH_WORKERS row = desired %q set=%v drift=%v, want \"6\"/true/true", row.Desired, row.DesiredSet, row.Drift)
	}
	if row.Value != "1" {
		t.Errorf("PALAI_DISPATCH_WORKERS effective = %q, want the process's own value \"1\" — the read must report "+
			"what this process HOLDS beside what was asked for, or the drift is unreadable", row.Value)
	}

	// --- 2. A REFUSED SETTING NEVER LANDS -------------------------------------------------------------
	put(t, ts.URL, `{"settings":{"PALAI_SECRET_MASTER_KEY_FILE":"/tmp/mine"}}`, http.StatusBadRequest)
	if got := revisionCount(t, repo); got != 1 {
		t.Errorf("the journal holds %d revisions after a REFUSED write; a 400 that still appended would make the "+
			"refusal cosmetic", got)
	}

	// --- 2b. THE RUNNER PLANE HAS A READER NOW, SO THE WRITE IS ACCEPTED ------------------------------
	// THIS ASSERTION AND ITS COMMENT BOTH USED TO SAY THE OPPOSITE, and the comment was the worse half —
	// it sat on the exact line a reader would check to learn why. It read: "Nothing hands cmd/runner a
	// document — it reads its environment at exec — so a runner_pool row would change no machine while
	// the screen reported a save." That sentence was true when it was written and `ffb84f3b` made it
	// false, so the refusal it justified became the regression.
	//
	// WHAT IS TRUE NOW, BY BRANCH, because "it works now" is the kind of sentence this comment is being
	// punished for:
	//   * A MACHINE ENROLLING receives its pool's document in the answer to its own enrolment —
	//     RunnerGateway.settingsFor (execution/runner_gateway.go:787) reads DesiredSettingsForPool and
	//     returns it with the identity, and cmd/runner's planeIntDefault PREFERS it over the machine's
	//     own environment. So a row written here does change a machine.
	//   * A MACHINE ALREADY RUNNING does NOT. Delivery rides enrolment and nothing pushes a later
	//     revision at a live runner; the gateway says so itself one comment up — "what is lost is a
	//     configuration the machine picks up on its next enrolment". Writing here and expecting a
	//     running fleet to change is still wrong, and that is a real limit rather than a bug.
	// The pool asked about comes from the RESOLVED GRANT and never from the request, so accepting this
	// write did not give one machine a way to read another pool's document.
	put(t, ts.URL, `{"plane":"runner_pool","scope_id":"pool_default","settings":{}}`, http.StatusOK)
	if got := revisionCount(t, repo); got != 2 {
		t.Errorf("the journal holds %d revisions after an ACCEPTED runner_pool write, want 2 — an accept that "+
			"appended nothing would make the 200 cosmetic, which is the same defect the refusal used to prevent "+
			"pointed the other way", got)
	}

	// --- 3. APPEND-ONLY, ENFORCED BY THE DATABASE -----------------------------------------------------
	// The runtime role is what the control plane serves on (storage.RuntimeRole, applied per acquisition),
	// so this is the privilege the process actually has — not the owner's.
	for _, statement := range []string{
		`UPDATE deployment_desired SET document = '{}'::jsonb WHERE revision = 1`,
		`DELETE FROM deployment_desired WHERE revision = 1`,
	} {
		if _, err := repo.Spine().Pool().Exec(storage.WithSystemScope(ctx), statement); err == nil {
			t.Errorf("the runtime role executed %q. A journal the process can restate or erase is not a journal, "+
				"and 000052's REVOKE is what makes 'we must be able to see it' a property rather than a promise", statement)
		}
	}

	// --- 4. THE CURRENT DOCUMENT IS THE HIGHEST REVISION ----------------------------------------------
	// A second write, and it REPLACES: PALAI_QUEUE_DEADLINE is absent from it, which is how an operator
	// stops deciding a setting and hands it back to the deployment's own default.
	// THE REVISION IS THE JOURNAL'S, NOT THIS PLANE'S, and this assertion used to hard-code `2` — a
	// literal that silently encoded "no other plane can ever write". That was safe only while the
	// runner_pool write was refused; the moment 2b started succeeding, the deployment's next write became
	// revision 3 without anything about the deployment document changing. So the number is READ rather
	// than pinned, and what is asserted is the property: the write advanced the journal.
	//
	// WORTH KNOWING, and it is a real consequence rather than a test detail: an operator watching the
	// revision on the deployment screen will see it move when somebody writes a runner_pool document,
	// because there is one append-only journal and `revision` is a position in it, not a per-plane count.
	before := deploymentRead(t, ts.URL).Desired.Revision
	put(t, ts.URL, `{"settings":{"PALAI_DISPATCH_WORKERS":"1"}}`, http.StatusOK)
	body = deploymentRead(t, ts.URL)
	if body.Desired.Revision <= before {
		t.Fatalf("revision after the second write = %d, want greater than %d — an accepted write that did not "+
			"advance the journal is a write nothing recorded", body.Desired.Revision, before)
	}
	if body.Desired.Pending {
		t.Errorf("desired now asks for exactly what this process holds and the surface still reports a pending "+
			"bring-up (%v). A banner that never clears is decoration", body.Desired.Drifted)
	}
	if row := settingRow(t, body, "PALAI_QUEUE_DEADLINE"); row.DesiredSet {
		t.Errorf("PALAI_QUEUE_DEADLINE is still reported as decided (%q) after a write that omitted it. The write "+
			"REPLACES the document, and removal is how a setting goes back to the deployment's own default — a "+
			"merge would make that inexpressible and the document could only ever grow", row.Desired)
	}
}

// --- the small helpers ------------------------------------------------------------------------------

type provisionVerifier struct{}

func (provisionVerifier) VerifyAPIKey(context.Context, string) (middleware.Scope, error) {
	return middleware.Scope{Project: "prj_local", Principal: "prin_local", Scopes: []string{"provision"}}, nil
}

func put(t *testing.T, base, body string, want int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, base+"/v1/deployment/desired", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer any")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /v1/deployment/desired: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("PUT %s = %d, want %d: %s", body, resp.StatusCode, want, raw)
	}
}

// desiredBody mirrors the two fields of api's response this test reads. It is a local shape rather than the
// api package's own, because those types are unexported — and pinning the JSON here is the stronger test
// anyway: it asserts the WIRE, which is what the console and `palai up` actually consume.
type desiredBody struct {
	Settings []struct {
		Name       string `json:"name"`
		Value      string `json:"value"`
		Desired    string `json:"desired"`
		DesiredSet bool   `json:"desired_set"`
		Drift      bool   `json:"drift"`
		Writable   bool   `json:"writable"`
	} `json:"settings"`
	Desired *struct {
		Plane    string   `json:"plane"`
		Scope    string   `json:"scope_id"`
		Revision int64    `json:"revision"`
		Pending  bool     `json:"pending"`
		Drifted  []string `json:"drifted"`
	} `json:"desired"`
}

func deploymentRead(t *testing.T, base string) desiredBody {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/deployment", nil)
	req.Header.Set("Authorization", "Bearer any")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/deployment: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/deployment = %d: %s", resp.StatusCode, raw)
	}
	var out desiredBody
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode deployment body: %v (%s)", err, raw)
	}
	return out
}

func settingRow(t *testing.T, body desiredBody, name string) struct {
	Name       string `json:"name"`
	Value      string `json:"value"`
	Desired    string `json:"desired"`
	DesiredSet bool   `json:"desired_set"`
	Drift      bool   `json:"drift"`
	Writable   bool   `json:"writable"`
} {
	t.Helper()
	for _, row := range body.Settings {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("GET /v1/deployment reports no %s row", name)
	panic("unreachable")
}

func revisionCount(t *testing.T, repo *Store) int {
	t.Helper()
	var n int
	if err := repo.Spine().Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM deployment_desired`).Scan(&n); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	return n
}
