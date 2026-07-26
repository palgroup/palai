// Command promote is the mechanical SH-2 promote gate (plan §7): a release cannot be tagged/promoted
// unless its evidence bundle verifies clean AND carries a rollback proof + a restore/DR proof. It backs
// `scripts/release/promote.sh` and exits non-zero on any refusal. A promote to `stable` additionally awaits
// the E14 §6 operator-leg attestation in the manifest (never auto-claimed).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/palgroup/palai/tests/uat"
)

func main() {
	release := flag.String("release", "", "release name under evidence/releases/")
	to := flag.String("to", "rc", "promote target: rc | stable")
	approval := flag.String("approval", "", "a palai.release-approval/v1 record (the protected environment's two-person approval); when given, the promotion is ALSO judged against release-policy.md's two-person rule")
	flag.Parse()
	if *release == "" || (*to != "rc" && *to != "stable") {
		fmt.Fprintln(os.Stderr, "usage: promote --release <name> [--to rc|stable]")
		os.Exit(2)
	}
	dir := filepath.Join("evidence", "releases", *release)

	var secrets []string
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		secrets = append(secrets, v) // opaque redaction needle, never printed
	}

	// A release must first VERIFY CLEAN — a bundle that does not pass the evidence contract cannot be promoted.
	summary, err := uat.VerifyRelease(dir, secrets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promote REFUSED: verify %s: %v\n", *release, err)
		os.Exit(1)
	}
	if !summary.OK() {
		fmt.Fprintf(os.Stderr, "promote REFUSED: %s did not verify clean: %s\n", *release, summary.String())
		for _, f := range summary.Findings {
			fmt.Fprintln(os.Stderr, "  - "+f.String())
		}
		os.Exit(1)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "promote REFUSED: read manifest: %v\n", err)
		os.Exit(1)
	}
	if refusals := uat.PromoteGateFor(raw, *to); len(refusals) > 0 {
		fmt.Fprintf(os.Stderr, "promote REFUSED: %s cannot be promoted to %s:\n", *release, *to)
		for _, r := range refusals {
			fmt.Fprintln(os.Stderr, "  - "+r.String())
		}
		os.Exit(1)
	}

	// The TWO-PERSON gate (E18 T5) runs LAST, on purpose: a fence in front of the evidence gate would
	// shadow the operator-leg refusal scripts/uat/sh2 and scripts/uat/sdk-parity grep for. It applies only
	// when an approval is presented, because presenting one is what a PUBLICATION does
	// (scripts/release/publish.sh always does; a local `promote.sh rc` is the evidence gate, nothing more).
	if *approval == "" {
		fmt.Printf("promote-gate PASS: %s may be tagged (%s) — verified clean, the release-family exit proofs present."+
			" NO two-person approval was presented: this is the evidence gate only, NOT an approved publication"+
			" (release-policy.md two-person promotion; scripts/release/publish.sh is the gated act)\n", *release, *to)
		return
	}
	record, err := os.ReadFile(*approval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promote REFUSED: read the approval record: %v\n", err)
		os.Exit(1)
	}
	maintainers, err := uat.MaintainersFromCODEOWNERS(filepath.Join(".github", "CODEOWNERS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "promote REFUSED: %v (the authorized set is recomputed from the canonical owners file, never taken from the approval)\n", err)
		os.Exit(1)
	}
	if refusals := uat.ApprovalGate(record, *release, *to, maintainers); len(refusals) > 0 {
		fmt.Fprintf(os.Stderr, "promote REFUSED: the two-person approval for %s (%s) does not hold:\n", *release, *to)
		for _, r := range refusals {
			fmt.Fprintln(os.Stderr, "  - "+r.String())
		}
		os.Exit(1)
	}
	fmt.Printf("promote-gate PASS: %s may be tagged (%s) — verified clean, the release-family exit proofs present,"+
		" and approved by an authorized maintainer OTHER than the builder in the protected %q environment\n",
		*release, *to, uat.ReleaseEnvironment)
}
