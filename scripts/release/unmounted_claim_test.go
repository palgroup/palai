package release

// THE CLAIM A SHIPPED RELEASE MAKES ABOUT THIS DIRECTORY, checked against the directory.
//
// ‼️ SIX TESTS NAME PALAI_CAPABILITY_WORKER_LISTEN_ADDR AND NOT ONE OF THEM READS deploy/. Measured
// 2026-08-06: tests/uat/{extensions,wiring,stable-release} and apps/control-plane/api all assert that the
// RC manifest's UnmountedReason CONTAINS the variable's name and is well-formed — the SENTENCE, not the
// world it describes. The committed release-1.0.0-rc1 manifest says, verbatim:
//
//	"no shipped deployment config sets PALAI_CAPABILITY_WORKER_LISTEN_ADDR (deploy/compose, deploy/helm
//	 and the production overlay all leave it unset), so the capability-worker gateway is not mounted in
//	 any deployment and no deployed binary serves this map"
//
// Adding that variable to a compose file tomorrow would leave all six green and make a shipped release's
// own sentence false: a deployment would serve a capability map the release states no deployed binary
// serves. The plan that removed the feature wrote the hazard down in as many words — "HİÇBİR TEST
// deploy/'u grep'lemiyor, yani gate YEŞİL kalırdı" (phase-22 §3.6 D13) — and left it unguarded.
//
// This is the guard. It is not a rule against mounting the gateway: it is a rule that mounting it and
// leaving the release's sentence in place cannot both happen quietly. Whoever wants the remote-worker
// path pays the price §5 of that plan records, and this test is where they find out.
//
// ‼️ IT LIVES HERE AND NOT UNDER deploy/, WHICH ITS FIRST DRAFT DID. `airgap-build.sh` refuses to sign a
// dirty deploy/compose, deploy/helm or storage/migrations — deliberately, so a stray local file is never
// signed and shipped — so a new file there turns `make verify` red until it is committed, and every test
// added beside the deployment files raises the cost of that rule for no reason. A guard about a RELEASE's
// claim belongs with the release guards; deploy/ is the data it reads.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unmountedInEveryDeployment is the variable the shipped manifest says no deployment config sets.
const unmountedInEveryDeployment = "PALAI_CAPABILITY_WORKER_LISTEN_ADDR"

// deployTree returns every deployment file, keyed by repo-relative path.
func deployTree(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join(repoRoot(t), "deploy")
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// ‼️ CONFIG FILES, NOT SOURCES. The claim is about what a DEPLOYMENT sets — compose files, helm
		// templates, env samples, overlays — and Go sources under deploy/ are this package's own tests. The
		// first run of this guard failed on ITSELF, because the file naming the variable in order to forbid
		// it is under deploy/ too. A corpus that includes the thing doing the measuring measures the wrong
		// world.
		if strings.HasSuffix(path, ".go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("deploy/ read as empty — a scan over no files reports the same clean result as a tree that sets nothing")
	}
	return files
}

// TestNoDeploymentMountsTheCapabilityWorkerGateway is the claim, measured.
//
// The positive control matters as much as the assertion: the same walk must find a variable these files
// DO set, or it is reading the wrong tree and its silence about the other one means nothing.
func TestNoDeploymentMountsTheCapabilityWorkerGateway(t *testing.T) {
	files := deployTree(t)

	var sawSomeSetting bool
	for _, body := range files {
		if strings.Contains(body, "PALAI_RUNNER_LISTEN_ADDR") {
			sawSomeSetting = true
			break
		}
	}
	if !sawSomeSetting {
		t.Fatal("the walk found no PALAI_RUNNER_LISTEN_ADDR anywhere under deploy/, which the compose stack " +
			"certainly sets — it is reading the wrong files, and the assertion below would pass on nothing")
	}

	for path, body := range files {
		if !strings.Contains(body, unmountedInEveryDeployment) {
			continue
		}
		if rel, err := filepath.Rel(repoRoot(t), path); err == nil {
			path = rel
		}
		t.Errorf("%s names %s. A shipped release manifest states that NO deployment config sets it and that "+
			"no deployed binary serves the capability map — mounting the gateway makes that sentence false, "+
			"and every test that checks the sentence would stay green. Mount it deliberately: correct the "+
			"release's claim in the same change, or leave it unset.", path, unmountedInEveryDeployment)
	}
}
