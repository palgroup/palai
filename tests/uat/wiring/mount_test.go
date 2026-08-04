package wiring

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// The E19 MOUNT-DERIVATION anti-fabrication anchor (plan §T9), as a refusal matrix.
//
// Every negative below hands the verifier a SHAPE-CONSISTENT manifest — one that would pass every other
// gate in the tree — carrying a mount it did not earn, and asserts it is REFUSED. A gate that has never
// been shown refusing is not a gate; this file is the evidence that it does.
//
// The defect they are modelled on is real and shipped: `capability-workers` was advertised at the STRONGEST
// tier by a control-plane binary that did not so much as import the worker gateway package (§3.5 D14). Every
// arm here is a way of reproducing that shape.

// committedBundle reads the wiring bundle, which every negative mutates. Reading the COMMITTED bytes rather
// than regenerating matters: the negatives are then proven against the artifact that actually ships.
func committedBundle(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the committed wiring bundle: %v", err)
	}
	if findings := uat.VerifyManifest(raw, nil); len(findings) != 0 {
		t.Fatalf("the committed bundle must verify clean before it is mutated: %v", findings)
	}
	return raw
}

// mutateWiringProof decodes the bundle, applies fn to the wiring proof, and re-encodes. The result is a
// manifest that is structurally valid JSON with one fact changed — the shape a fabricator would produce.
func mutateWiringProof(t *testing.T, raw []byte, fn func(*uat.WiringProof)) []byte {
	t.Helper()
	var m struct {
		Cases []map[string]json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the bundle document: %v", err)
	}
	found := false
	for i, c := range m.Cases {
		blob, ok := c["wiring_proof"]
		if !ok {
			continue
		}
		var proof uat.WiringProof
		if err := json.Unmarshal(blob, &proof); err != nil {
			t.Fatalf("decode the wiring proof: %v", err)
		}
		fn(&proof)
		next, err := json.Marshal(proof)
		if err != nil {
			t.Fatalf("re-encode the wiring proof: %v", err)
		}
		m.Cases[i]["wiring_proof"] = next
		found = true
	}
	if !found {
		t.Fatal("the bundle carries no wiring_proof to mutate")
	}
	cases, err := json.Marshal(m.Cases)
	if err != nil {
		t.Fatalf("re-encode the cases: %v", err)
	}
	doc["cases"] = cases
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode the document: %v", err)
	}
	return out
}

func hasDetail(findings []uat.Finding, needle string) bool {
	for _, f := range findings {
		if strings.Contains(f.Detail, needle) {
			return true
		}
	}
	return false
}

func hasRefusal(refusals []uat.Refusal, needle string) bool {
	for _, r := range refusals {
		if strings.Contains(r.Detail, needle) {
			return true
		}
	}
	return false
}

// TestMountRefusals is the matrix. Each arm names the shape it reproduces.
func TestMountRefusals(t *testing.T) {
	base := committedBundle(t)

	for _, tc := range []struct {
		name   string
		mutate func(*uat.WiringProof)
		want   string
	}{
		{
			// THE D14 DEFECT VERBATIM: the capability is advertised, the route is not in the router
			// surface. This is `capability-workers` claiming "stable" from a binary with no gateway.
			name: "advertised but not mounted",
			mutate: func(p *uat.WiringProof) {
				p.RouterSurface = dropRoute(p.RouterSurface, "POST /v1/a2a/interfaces/{interface_id}/tasks/{id}/pushNotificationConfigs")
			},
			want: "router surface does not contain it",
		},
		{
			// THE SAME LIE INVERTED: the surface is mounted and serving, and discovery does not declare
			// it. Discovery is supposed to be a function of the mount, and a function is total.
			name: "mounted but not advertised",
			mutate: func(p *uat.WiringProof) {
				delete(p.CapabilitySnapshot, "a2a")
			},
			want: "does not advertise",
		},
		{
			// A NEGATIVE CLAIM CONTRADICTED BY A MOUNT: `disabled` says "this deployment cannot serve
			// it". Serving it anyway is the most dangerous shape of all, because a reader trusts the word.
			name: "advertised disabled while mounted",
			mutate: func(p *uat.WiringProof) {
				p.CapabilitySnapshot["a2a"] = "disabled"
			},
			want: "a disabled entry is a NEGATIVE claim",
		},
		{
			// A ROUTE OBSERVED AS 404 IS AN UNMOUNTED ROUTE. Recording the observation honestly and
			// claiming the surface anyway is the fabrication the observed_status field exists to catch.
			name: "route observed 404",
			mutate: func(p *uat.WiringProof) {
				setSurface(p, "a2a-push", func(s *uat.WiredSurface) { s.ObservedStatus = 404 })
			},
			want: "wiring_proof is incomplete",
		},
		{
			// A SUPERVISED LOOP HAS NO STATUS TO ANSWER. Inventing one is an observation that could not
			// have been taken.
			name: "supervised loop claims a status",
			mutate: func(p *uat.WiringProof) {
				setSurface(p, "queue-inbound", func(s *uat.WiredSurface) { s.ObservedStatus = 200 })
			},
			want: "wiring_proof is incomplete",
		},
		{
			// A DROPPED §3.5 ROW: the divergence table is the plan's crown output, so a surface that
			// silently stops accounting for D1 (the retry-amplification row) has reintroduced the gap.
			name: "shrunken contract ledger",
			mutate: func(p *uat.WiringProof) {
				setSurface(p, "a2a-push", func(s *uat.WiredSurface) { s.Contracts = s.Contracts[1:] })
			},
			want: "wiring_proof is incomplete",
		},
		{
			// A REWRITTEN SOURCE URL: grounding that points somewhere else is not grounding. The ledger
			// digest is over the CODE table, so this cannot stay self-consistent.
			name: "rewritten source url",
			mutate: func(p *uat.WiringProof) {
				setSurface(p, "a2a-push", func(s *uat.WiredSurface) {
					next := append([]uat.ContractRequirement(nil), s.Contracts...)
					next[0].SourceURL = "https://example.invalid/whatever"
					s.Contracts = next
				})
			},
			want: "wiring_proof is incomplete",
		},
		{
			// TRANSPORT INVARIANCE WITH NO DUPLICATE: one delivery of one event proves nothing about what
			// happens when the SAME event arrives twice, which is the entire claim.
			name: "no duplicate delivery",
			mutate: func(p *uat.WiringProof) {
				admitting(p, func(s *uat.WiredSurface) { s.Deliveries = 1 })
			},
			want: "wiring_proof is incomplete",
		},
		{
			// TWO RUNS FROM ONE SOURCE EVENT is the defect itself: a transport swap that opened a second
			// run.
			name: "two runs from one event",
			mutate: func(p *uat.WiringProof) {
				admitting(p, func(s *uat.WiredSurface) { s.AdmittedRuns = 2 })
			},
			want: "wiring_proof is incomplete",
		},
		{
			// ADMISSION WITH NO ROUTE CONSTANT: the idempotency namespace is the only mechanical evidence
			// the SHARED Admitter ran rather than a parallel path.
			name: "admission with no route constant",
			mutate: func(p *uat.WiringProof) {
				admitting(p, func(s *uat.WiredSurface) { s.AdmissionRoute = "" })
			},
			want: "wiring_proof is incomplete",
		},
		{
			// A CLAIMED REAL COUNTERPARTY. No code in this repository can produce that receipt.
			name:   "claims a real peer",
			mutate: func(p *uat.WiringProof) { p.Peers = "real-slack-workspace" },
			want:   "wiring_proof is incomplete",
		},
		{
			// A DROPPED SURFACE: omission is the cheapest way to dodge a check.
			name:   "dropped surface",
			mutate: func(p *uat.WiringProof) { p.Surfaces = p.Surfaces[1:] },
			want:   "wiring_proof is incomplete",
		},
		{
			// A LIVE LEG THAT DOES NOT SKIP: one that FAILS turns a partial handover into a red wall; one
			// that PASSES is asserting something it never ran.
			name: "live leg does not skip",
			mutate: func(p *uat.WiringProof) {
				next := append([]uat.LiveLeg(nil), p.LiveLegs...)
				next[0].WithoutCredential = "pass"
				p.LiveLegs = next
			},
			want: "wiring_proof is incomplete",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := mutateWiringProof(t, base, tc.mutate)
			findings := uat.VerifyManifest(tampered, nil)
			if !hasDetail(findings, tc.want) {
				t.Fatalf("the verifier did not refuse %q (want a finding containing %q): %v", tc.name, tc.want, findings)
			}
			// The promote gate must refuse independently — it does its OWN derivation rather than
			// inheriting the verifier's verdict.
			if refusals := uat.PromoteGateFor(tampered, "rc"); len(refusals) == 0 {
				t.Fatalf("the promote gate PASSED a bundle the verifier refused for %q — the gate must derive, not inherit", tc.name)
			}
		})
	}
}

// TestDroppingTheWiringClaimDoesNotSwitchTheAnchorOff is the "family recognized by the claim the gate
// enforces" defect, in its E19 form. A wiring bundle also carries E17 area claims, so a release that DROPPED
// its wiring claim would reroute to ExtensionsPromoteGate — which knows nothing about mounts — and pass.
//
// The bundle stays honest here only because the mount guard would then not run at all, which is precisely
// why PromoteGateFor checks the E19 family FIRST. This test pins the dispatch, not the manifest.
func TestDroppingTheWiringClaimReroutesAndIsVisible(t *testing.T) {
	base := committedBundle(t)
	stripped := bytes.Replace(base, []byte(`"wiring_claim"`), []byte(`"wiring_claim_dropped"`), 1)
	if bytes.Equal(stripped, base) {
		t.Fatal("the bundle carries no wiring_claim to drop")
	}
	// With the marker gone the wiring gate no longer governs. That is exactly the hole, so the assertion is
	// that the DISPATCH is what closes it: with the marker PRESENT, the wiring gate must be the one running.
	// A mount-broken bundle proves which gate judged it.
	broken := mutateWiringProof(t, base, func(p *uat.WiringProof) {
		p.RouterSurface = dropRoute(p.RouterSurface, "POST /v1/a2a/interfaces/{interface_id}/tasks/{id}/pushNotificationConfigs")
	})
	if !hasRefusal(uat.PromoteGateFor(broken, "rc"), "router surface does not contain it") {
		t.Fatal("a mount-broken bundle WITH its wiring claim was not judged by the wiring gate — PromoteGateFor must dispatch E19 before E17, or the mount guard is optional in practice")
	}
	strippedBroken := bytes.Replace(broken, []byte(`"wiring_claim"`), []byte(`"wiring_claim_dropped"`), 1)
	if hasRefusal(uat.PromoteGateFor(strippedBroken, "rc"), "router surface does not contain it") {
		t.Fatal("the mount refusal survived the claim being dropped — then this test proves nothing about dispatch")
	}
	t.Log("dispatch confirmed: the mount refusal exists ONLY while the wiring claim selects the E19 gate, which is why PromoteGateFor checks that family first")
}

// TestWiringGateRefusesATierThatAdvanced is clause 2 of the promote gate, shown REFUSING. E19's defining
// rule is that wiring moves no tier, so the gate compares this release's recompute against the committed
// E17 baseline — and the arm below is what makes that comparison non-vacuous: it removes the §6 leg that
// caps `slack`, which is exactly what a future operator leg will do, and asserts the WIRING release is
// refused for advancing.
func TestWiringGateRefusesATierThatAdvanced(t *testing.T) {
	base := committedBundle(t)
	if refusals := uat.PromoteGateFor(base, "rc"); len(refusals) != 0 {
		t.Fatalf("the committed wiring bundle must PASS its own rc gate first: %v", refusals)
	}

	// A capability whose §6 leg is gone recomputes to stable — the shape a FUTURE release takes once an
	// operator leg closes. The declaration and the snapshot are moved with it, because otherwise the
	// composed extensions gate refuses first (declared != recompute) and clause 2 would never be reached:
	// an arm that passes on someone else's refusal proves nothing about the guard it names. This mutation
	// therefore produces a bundle that is INTERNALLY CONSISTENT and still must be refused, because a
	// wiring release is not where a tier moves — that is the whole clause.
	saved := uat.CapabilityOperatorLegs["slack"]
	delete(uat.CapabilityOperatorLegs, "slack")
	t.Cleanup(func() { uat.CapabilityOperatorLegs["slack"] = saved })

	advanced := bytes.ReplaceAll(base,
		[]byte("\"capability\": \"slack\",\n            \"declared_tier\": \"preview\""),
		[]byte("\"capability\": \"slack\",\n            \"declared_tier\": \"stable\""))
	advanced = bytes.ReplaceAll(advanced, []byte(`"slack": "preview"`), []byte(`"slack": "stable"`))
	if bytes.Equal(advanced, base) {
		t.Fatal("the tier declaration/snapshot could not be advanced; the mutation no longer matches the bundle's shape")
	}

	refusals := uat.PromoteGateFor(advanced, "rc")
	if !hasRefusal(refusals, `recomputes to "stable" in this WIRING release`) {
		t.Fatalf("a tier that ADVANCED in a wiring release was not refused — clause 2 is vacuous: %v", refusals)
	}
	// And it is refused for THAT reason, not incidentally: the composed extensions gate must be silent on
	// an internally consistent table.
	if hasRefusal(refusals, "never a declaration") {
		t.Fatalf("the extensions gate fired too, so this arm does not isolate clause 2: %v", refusals)
	}
	t.Logf("REFUSED as required: %v", refusals)
}

// TestCommittedWiringBundlePassesItsOwnGate is the positive control the refusal matrix needs: without it,
// "everything is refused" would satisfy every negative above.
func TestCommittedWiringBundlePassesItsOwnGate(t *testing.T) {
	base := committedBundle(t)
	if refusals := uat.PromoteGateFor(base, "rc"); len(refusals) != 0 {
		t.Fatalf("the committed wiring bundle does not pass its own rc promote gate: %v", refusals)
	}
	// A promote BEYOND rc is refused: no amount of wiring earns stable, because every §6 leg is untouched.
	stable := uat.PromoteGateFor(base, "stable")
	if len(stable) == 0 {
		t.Fatal("a promote to STABLE was accepted — E19 contacted no counterparty and cannot promote anything")
	}
	t.Logf("stable REFUSED as required: %v", stable)
}

func dropRoute(routes []string, drop string) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		if r != drop {
			out = append(out, r)
		}
	}
	return out
}

// admitting gives queue-inbound a VALID transport-invariance counter and then applies fn to break it.
//
// IT SYNTHESISES THE COUNTER BECAUSE NO SURFACE DECLARES ONE ANY MORE. Until the 2026-08-05 cutover the
// three negatives below mutated slack-events, which carried the bundle's only counter: one source event id,
// two deliveries, one run. Slack is a separate process now and that surface is gone from the canonical set.
//
// SYNTHESISING IS LEGITIMATE HERE AND WOULD NOT BE IN THE BUNDLE, and the difference is what is under test.
// A refusal matrix tests the VERIFIER: it takes a valid proof, breaks it one way, and requires a finding. It
// has always manufactured its inputs — that is what `mutate` is. What may never be manufactured is an
// OBSERVATION a committed bundle carries, and this function is not reachable from the generator.
//
// Without it these three cases would have been quietly deleted or left mutating a field no surface has,
// where they would have passed by never firing — a vacuous refusal, which is worse than an absent one
// because it reports as coverage.
func admitting(p *uat.WiringProof, fn func(*uat.WiredSurface)) {
	setSurface(p, "queue-inbound", func(s *uat.WiredSurface) {
		s.AdmissionRoute = "/v1/responses"
		s.SourceEventIDs = []string{"EvQueue1"}
		s.Deliveries = 2
		s.AdmittedRuns = 1
		fn(s)
	})
}

func setSurface(p *uat.WiringProof, name string, fn func(*uat.WiredSurface)) {
	for i := range p.Surfaces {
		if p.Surfaces[i].Surface == name {
			fn(&p.Surfaces[i])
			return
		}
	}
	panic("no such surface: " + name)
}
