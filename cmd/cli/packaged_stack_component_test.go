//go:build component

// The bring-up half of the outside-the-tree contract. packaged_test.go proves the RESOLUTION —
// which compose file, which images, whether --build is passed — against a recording stub. This file
// proves the thing that resolution exists for: a `palai` binary whose working directory has no
// `deploy/` at any level above it brings up a real four-service stack, on real Docker, and every
// doctor check goes green.
//
// It needs images, and NO Palai image has ever been published (release.yml's publish step is the
// credential-less dry run, plan §6 leg 2) — so the suite script builds them locally and names them
// in PALAI_PACKAGED_IMAGES. That is the whole gap between this test and the operator's day one: on
// the day images are published the same test runs against published refs and only the tags change.
//
// Run it with `make test-component TEST=packaged`. Without PALAI_PACKAGED_IMAGES it FAILS rather
// than skipping: a bring-up proof that quietly passes on a machine with no images is worse than no
// proof, because it reads as one.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackagedStackComesUpOutsideTheTree(t *testing.T) {
	refs := strings.Split(os.Getenv("PALAI_PACKAGED_IMAGES"), ",")
	if len(refs) != 3 || refs[0] == "" {
		t.Fatal("PALAI_PACKAGED_IMAGES must name control-plane,runner,reference-engine — run `make test-component TEST=packaged`, which builds them")
	}

	bin := cli(t)
	work := outsideTheTree(t)
	home := filepath.Join(work, ".palai")
	env := []string{"PALAI_HOME=" + home}

	if out, err := palai(t, bin, work, env, "init"); err != nil {
		t.Fatalf("palai init outside the tree: %v\n%s", err, out)
	}
	// The engine ref is handed over as a TAG on purpose: the runner's lease validator accepts only a
	// bare sha256 config digest, so a packaged bring-up that passed the tag through would come up
	// healthy here and fail every run afterwards. Passing the tag is what exercises the resolution.
	up := append(env,
		"PALAI_CONTROL_PLANE_IMAGE="+refs[0],
		"PALAI_RUNNER_IMAGE="+refs[1],
		"PALAI_ENGINE_IMAGE="+refs[2],
	)
	t.Cleanup(func() {
		// Deletes this stack's volumes and sweeps its engine sandboxes; it is scoped to the compose
		// project `init` minted, so a concurrent stack on this host is untouched.
		if out, err := palai(t, bin, work, up, "local", "reset", "--confirm"); err != nil {
			t.Errorf("tear the packaged stack down: %v\n%s", err, out)
		}
	})

	if out, err := palai(t, bin, work, up, "local", "up"); err != nil {
		t.Fatalf("packaged `local up` outside the tree: %v\n%s", err, out)
	}

	out, err := palai(t, bin, work, env, "local", "doctor", "--json")
	if err != nil {
		t.Fatalf("packaged `local doctor`: %v\n%s", err, out)
	}
	// doctor --json always exits 0 — the verdict is in the body, so the status of every check is
	// read rather than inferred from the exit code.
	var report struct {
		OK     bool `json:"ok"`
		Checks map[string]struct {
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode the doctor report: %v\n%s", err, out)
	}
	if len(report.Checks) == 0 {
		t.Fatalf("doctor reported no checks at all — a green verdict over an empty map proves nothing:\n%s", out)
	}
	for name, c := range report.Checks {
		if c.Status == "ok" {
			continue
		}
		// `disk` is the ONE check whose verdict is about the HOST rather than about the stack this
		// test brought up, and a machine below the floor would red it for every deployment equally.
		// The exemption is deliberately narrow: the check must still have RUN, and it must be red
		// for exactly that reason — a disk check red for any other reason still fails the test, and
		// no other check is exempt at all.
		if name == "disk" && strings.Contains(c.Detail, "under the 10% floor") {
			t.Logf("HOST, not the stack: %s (free space is a property of this machine; every other check is asserted green)", c.Detail)
			continue
		}
		t.Errorf("doctor check %s is %s: %s", name, c.Status, c.Detail)
	}
}
