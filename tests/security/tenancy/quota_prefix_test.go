//go:build security

// Package tenancy, continued (A.6 Task 1): `meter_prefix` is a PREFIX, not a LIKE pattern.
//
// The column is written by the CALLER. POST /v1/budgets and POST /v1/quotas take `meter_prefix` from the
// request body and pass it through verbatim (apps/control-plane/internal/metering/store.go, SetBudget /
// SetQuota); only the SCOPE is taken from the verified identity. So the value the two admission queries
// interpolate is tenant text, and what it MEANS is this corpus's business.
//
// Under `LIKE meter_prefix || '%'` it meant a pattern: `%` matched every meter and `_` matched any single
// character, so a limit written for `model_` silently covered `model.input_tokens`. Both directions only
// ever WIDEN the matched set — a wildcard makes a cap sum more meters and therefore bite sooner, so this
// was a correctness defect rather than a way past a limit, and the query's own header said so. It stayed a
// defect worth closing because the fix removes the reader's obligation to know LIKE's escaping rules at
// all: a prefix that is compared as a prefix has no wildcards to escape.
//
// The corpus drives the SHIPPED statements through storage.Query on the app role inside the project's own
// scope — the same connection shape packages/coordinator's checkDurableLimits runs in. A copy of the SQL
// written here would prove this file, not the control plane.
package tenancy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// meterFact is one settled ledger row the limits below are measured against.
type meterFact struct {
	meter    string
	quantity int
}

// seedPrefixCase creates a fresh project carrying one budget and one quota on the SAME meter prefix, plus
// the ledger rows they are asked about, and returns the project.
//
// limit_quantity is 1 while every fact is 100, so "is this limit exhausted" is decided entirely by whether
// the prefix MATCHED: one matched row exhausts it, zero matched rows leave it untouched. That is what lets
// a query returning no row be read as "the prefix counted nothing" rather than as an arithmetic accident.
//
// period_start is pushed an hour into the past explicitly. It defaults to clock_timestamp(), the budget is
// inserted BEFORE the ledger rows, and the join takes only `l.occurred_at >= b.period_start` — a budget
// stamped after the spend it is asked about counts none of it, and every negative arm here would then pass
// for that reason instead of its own.
func (s *suite) seedPrefixCase(t *testing.T, prefix string, facts []meterFact) string {
	t.Helper()
	ctx := context.Background()
	project := newID("prj")
	if _, err := s.owner.Exec(ctx, `INSERT INTO projects (id) VALUES ($1)`, project); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := s.owner.Exec(ctx,
		`INSERT INTO budgets (id, project_id, meter_prefix, limit_quantity, period_start)
		 VALUES ($1, $2, $3, 1, now() - interval '1 hour')`,
		newID("bdg"), project, prefix); err != nil {
		t.Fatalf("seed budget on prefix %q: %v", prefix, err)
	}
	if _, err := s.owner.Exec(ctx,
		`INSERT INTO quotas (id, project_id, meter_prefix, limit_quantity, window_seconds)
		 VALUES ($1, $2, $3, 1, 3600)`,
		newID("quo"), project, prefix); err != nil {
		t.Fatalf("seed quota on prefix %q: %v", prefix, err)
	}
	var seeded float64
	for _, f := range facts {
		id := newID("usg")
		if _, err := s.owner.Exec(ctx,
			`INSERT INTO usage_ledger (id, project_id, meter, quantity, unit, dedupe_key)
			 VALUES ($1, $2, $3, $4, 'token', $1)`,
			id, project, f.meter, f.quantity); err != nil {
			t.Fatalf("seed ledger row %s: %v", f.meter, err)
		}
		seeded += float64(f.quantity)
	}
	// THE FIXTURE PROVES ITSELF. Two of the four cases below assert that NO limit came back, and a project
	// with no ledger rows at all answers that just as well — so an INSERT that silently landed nowhere
	// would turn this corpus green for a reason with nothing to do with prefix matching.
	var stored float64
	if err := s.owner.QueryRow(ctx,
		`SELECT coalesce(sum(quantity), 0) FROM usage_ledger WHERE project_id = $1`, project).Scan(&stored); err != nil {
		t.Fatalf("read back the seeded ledger: %v", err)
	}
	if stored != seeded {
		t.Fatalf("ledger fixture for %s holds %v units, the case seeded %v", project, stored, seeded)
	}
	return project
}

// exhaustedLimit runs the shipped ExhaustedBudget/ExhaustedQuota against project, as the runtime role
// inside that project's scope, and reports which limit (if any) refused.
func (s *suite) exhaustedLimit(t *testing.T, kind, project string) (prefix string, used float64, found bool) {
	t.Helper()
	s.asProject(t, project, func(tx pgx.Tx) {
		var limit float64
		var err error
		switch kind {
		case "budget":
			var periodStart time.Time
			err = tx.QueryRow(context.Background(), storage.Query("ExhaustedBudget"), project).
				Scan(&prefix, &limit, &used, &periodStart)
		case "quota":
			var windowSeconds int64
			var oldest *time.Time
			err = tx.QueryRow(context.Background(), storage.Query("ExhaustedQuota"), project).
				Scan(&prefix, &limit, &used, &windowSeconds, &oldest)
		default:
			t.Fatalf("unknown limit kind %q", kind)
		}
		switch {
		case err == nil:
			found = true
		case errors.Is(err, pgx.ErrNoRows):
			found = false
		default:
			t.Fatalf("read exhausted %s: %v", kind, err)
		}
	})
	return prefix, used, found
}

// TestMeterPrefixIsALiteralPrefixNotAPattern drives every corner against BOTH limits, because the two
// queries carry the same join predicate and a fix applied to one leaves the other half shipped.
//
// The three negative cases and the two positive ones are not decoration for each other. `%`, `_` and `\`
// prove the metacharacters are gone — and there are THREE of them, where the header this task replaced
// named two; a corpus that stopped at the plan's `%` and `_` would have left LIKE's own escape character
// unmeasured. The positive cases prove the fix did not achieve that by breaking prefix matching outright:
// without the literal-underscore case, a predicate that simply never matched would satisfy all three
// negatives, and without the empty-prefix case an installation-wide cap on everything would silently
// become a cap on nothing.
func TestMeterPrefixIsALiteralPrefixNotAPattern(t *testing.T) {
	s := newSuite(t)

	cases := []struct {
		name      string
		prefix    string
		facts     []meterFact
		wantFound bool
		wantUsed  float64
		why       string
	}{
		{
			name:      "a percent prefix covers the meters beginning with a percent, which is none",
			prefix:    "%",
			facts:     []meterFact{{meter: "model.input_tokens", quantity: 100}},
			wantFound: false,
			why:       "under LIKE the pattern becomes `%%`, which matches EVERY meter — a tenant widens its own limit to all of its spend by writing one character",
		},
		{
			name:      "an underscore is a character, not any character",
			prefix:    "model_",
			facts:     []meterFact{{meter: "model.input_tokens", quantity: 100}},
			wantFound: false,
			why:       "under LIKE the `_` matches the `.` in model.input_tokens, so a limit written for `model_…` silently governs `model.…` — and the prefix looks deliberate, which is what makes this the sinister direction",
		},
		{
			name:      "a backslash is a character, not an escape",
			prefix:    `model\.`,
			facts:     []meterFact{{meter: "model.input_tokens", quantity: 100}},
			wantFound: false,
			why:       "LIKE has THREE metacharacters, not the two the old header named: `\\` is its default escape, so the pattern `model\\.%` reads as a literal `.` and matches model.input_tokens — a prefix that looks like it was carefully escaped BY someone matched the very thing they escaped it away from",
		},
		{
			name:      "a literal underscore still matches itself, so a prefix is still a prefix",
			prefix:    "model_",
			facts:     []meterFact{{meter: "model_legacy_tokens", quantity: 100}},
			wantFound: true,
			wantUsed:  100,
			why:       "removing the wildcards must not remove the matching: this is the same prefix as the case above, over the meter it was actually written for",
		},
		{
			name:      "the empty prefix still covers every meter",
			prefix:    "",
			facts:     []meterFact{{meter: "machine.minutes", quantity: 100}},
			wantFound: true,
			wantUsed:  100,
			why:       "left(meter, 0) = '' holds on every row, which is the contract budgets and quotas shipped with — an installation-wide cap on everything is written as an empty prefix",
		},
	}

	for _, tc := range cases {
		for _, kind := range []string{"budget", "quota"} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				project := s.seedPrefixCase(t, tc.prefix, tc.facts)
				prefix, used, found := s.exhaustedLimit(t, kind, project)
				// A limit whose prefix is not this case's belongs to another row in this shared database
				// (an installation-wide one seeded by a neighbouring test would be reported for this
				// project too). Naming that separately keeps a polluted fixture from reading as the
				// wildcard defect.
				if found && prefix != tc.prefix {
					t.Fatalf("the %s reported for this project has prefix %q, not this case's %q: another limit in this database was exhausted, so this case measured nothing",
						kind, prefix, tc.prefix)
				}
				switch {
				case found && !tc.wantFound:
					t.Fatalf("meter prefix %q exhausted the %s by counting %v units of meters it does not literally prefix — %s",
						tc.prefix, kind, used, tc.why)
				case !found && tc.wantFound:
					t.Fatalf("meter prefix %q counted nothing and left the %s with headroom — %s",
						tc.prefix, kind, tc.why)
				case found && used != tc.wantUsed:
					t.Fatalf("meter prefix %q counted %v units against the %s, want %v — the prefix matched a different set of meters than the case seeded",
						tc.prefix, used, kind, tc.wantUsed)
				}
			})
		}
	}
}
