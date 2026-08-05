package api

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// A.6 Task 2 — the 429 says WHICH ceiling refused the caller, and says nothing about who else shares it.
//
// These two properties are one test file because they are in tension, and a change that satisfies either
// one alone is the wrong change. The refusal has to become MORE specific (a project operator must be able
// to tell "raise your own quota" from "the shared pool is drained", which the old body could not) without
// becoming specific about the OTHER TENANTS behind that pool. An installation-wide limit sums every
// project's settled spend, so the naive way to explain a refusal to the project that spent nothing is to
// say who did spend — and that sentence hands one customer another customer's existence and volume.
//
// The scope word is the whole remediation. Before this task the body read `the quota for meters starting
// with "model." is exhausted (110 of 100 used)`, and a caller reading it could not know whether raising
// its own quota changed anything: under a project row it does, under the installation-wide pool it does
// not and the numbers shown were never that caller's in the first place.

// limitScopeCases are the four bodies the gate can render: two scopes x two kinds. They are enumerated
// rather than sampled because the remediation clause differs on BOTH axes — a quota's capacity returns on
// its own, a budget's does not, and an installation-wide budget is not the reading project's to raise.
func limitScopeCases() (reset time.Time) {
	return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
}

// TestARefusalNamesItsScopeSoTheRemediationIsActionable drives detail() — the exact string
// middleware.WriteProblem carries into the §20.10 429 body from (*responseHandler).create, which is the
// only place in this package that renders a LimitExceeded.
func TestARefusalNamesItsScopeSoTheRemediationIsActionable(t *testing.T) {
	reset := limitScopeCases()
	for _, tc := range []struct {
		name     string
		limit    LimitExceeded
		contains []string
		absent   []string
	}{
		{
			name: "a project quota names the project scope and when capacity returns",
			limit: LimitExceeded{
				Kind: "quota", MeterPrefix: "model.", Limit: 40, Used: 40, ResetAt: &reset,
			},
			contains: []string{"project quota", `"model."`, "40 of 40", "capacity returns at 2026-08-05T12:00:00Z"},
			absent:   []string{"installation", "summed across"},
		},
		{
			name: "an installation-wide quota says the number is a sum, so a caller does not read it as its own",
			limit: LimitExceeded{
				Kind: "quota", MeterPrefix: "model.", Limit: 100, Used: 110, ResetAt: &reset,
				InstallationWide: true,
			},
			contains: []string{"installation-wide quota", `"model."`, "110 of 100", "summed across every project",
				"capacity returns at 2026-08-05T12:00:00Z"},
			absent: []string{"project quota"},
		},
		{
			// The remediation this tree already ships and a live test already pins: an unexhausted budget
			// does not return on its own, so the only way forward is a higher limit — and under a PROJECT
			// budget that is a thing the reader owns.
			name: "a project budget still tells the reader to raise it",
			limit: LimitExceeded{
				Kind: "budget", MeterPrefix: "model.", Limit: 5, Used: 42,
			},
			contains: []string{"project budget", `"model."`, "42 of 5", "raise the budget"},
			absent:   []string{"capacity returns", "installation"},
		},
		{
			// And the case that makes the scope word load-bearing rather than decorative: "raise the
			// budget" is FALSE advice here. The reading project cannot raise the installation's budget,
			// and a body that told it to would send an operator to a settings screen that changes nothing.
			name: "an installation-wide budget does not tell the reader to raise what is not theirs",
			limit: LimitExceeded{
				Kind: "budget", MeterPrefix: "model.", Limit: 600, Used: 650, InstallationWide: true,
			},
			contains: []string{"installation-wide budget", "650 of 600", "summed across every project",
				"not this project's to raise"},
			absent: []string{"raise the budget to admit", "capacity returns"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.limit.detail()
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("the 429 detail %q does not carry %q", got, want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(got, unwanted) {
					t.Fatalf("the 429 detail %q carries %q, which is wrong for this scope/kind", got, unwanted)
				}
			}
		})
	}
}

// tenantIdentifierShape matches this tree's id vocabulary — middleware.NewID renders a prefix, an
// underscore and hex (apps/control-plane/api/middleware/request_context.go), and every tenant-owned row
// this gate can see is keyed by one. The assertion below is written against the SHAPE rather than against
// a fixture's literal id on purpose: a check for one seeded string is satisfied by rendering a different
// project's id, which is the leak it was supposed to refuse.
var tenantIdentifierShape = regexp.MustCompile(`\b(prj|org|prin|key|ses|run|resp)_[0-9a-f]{4,}\b`)

// TestAnInstallationWideRefusalNamesNoOtherTenant is the boundary half, and it is a boundary rather than
// a nicety: the reading project spent NOTHING here, so every unit in `used` belongs to somebody else. A
// body that explained the refusal by naming the projects behind it — or that carried any tenant id at all
// — would tell one customer that another customer exists and how much it is spending, which is the
// leak this tree spends its time closing, stated out loud in an error message.
func TestAnInstallationWideRefusalNamesNoOtherTenant(t *testing.T) {
	reset := limitScopeCases()
	limit := LimitExceeded{
		Kind: "quota", MeterPrefix: "model.", Limit: 100, Used: 110, ResetAt: &reset, InstallationWide: true,
	}
	got := limit.detail()
	if found := tenantIdentifierShape.FindString(got); found != "" {
		t.Fatalf("the installation-wide 429 detail %q carries the tenant identifier %q — a refusal must not "+
			"tell one project which other project drained the shared pool", got, found)
	}
	// The pooled total is still reported, and that is the deliberate line: an aggregate the caller needs
	// in order to understand the refusal is not the same disclosure as the identity or the per-tenant
	// split behind it.
	if !strings.Contains(got, "110 of 100") {
		t.Fatalf("the installation-wide 429 detail %q does not report the pooled total; the caller cannot "+
			"tell an exhausted pool from a misconfigured one", got)
	}
}

// TestLimitExceededCarriesNoTenantFacts is the structural half, and it exists because the string test
// above can only refuse what today's renderer happens to print. This one refuses the FIELD: nothing on
// the struct that crosses into a caller-visible body may carry another tenant's identity, and the scope
// is therefore a BOOLEAN rather than the project_id the query could just as easily have returned. With a
// project_id here the non-leak would be a property of detail() remembering not to print it; with a
// boolean it is a property of the type.
//
// A new field is not forbidden — it is required to come past this list, where the question "can this hold
// a fact about a project other than the caller's?" gets asked once, deliberately, by whoever adds it.
func TestLimitExceededCarriesNoTenantFacts(t *testing.T) {
	want := []string{"InstallationWide", "Kind", "Limit", "MeterPrefix", "ResetAt", "Used"}
	typ := reflect.TypeOf(LimitExceeded{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LimitExceeded fields = %v, want %v.\nThis type is rendered into a 429 body that one "+
			"customer reads about a limit another customer may have drained. A field added here can carry a "+
			"fact about a project that is not the caller's — the scope is a bool for exactly that reason. "+
			"If the new field is safe, say so and add it to this list; if it names a tenant, it does not "+
			"belong on the type at all.", got, want)
	}
}
