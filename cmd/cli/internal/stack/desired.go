package stack

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// THE BRING-UP READER. This file is the answer to the one question the desired-configuration surface
// cannot answer about itself: WHO reads the document and turns it into the process's environment.
//
// Without it the panel writes to a table nobody consults, which is worse than the dotenv file it replaces
// because the file at least worked. So the claim is made here and proven by `palai up` itself: the command
// applies the document and then REFUSES to report success on a stack whose effective configuration still
// disagrees with it. That is the same discipline up.go already applies to the model round trip.
//
// THE SEQUENCING, and it is the whole design of this file.
//
//	[3/6] BEFORE the bring-up: read the desired document off the control plane that is ALREADY RUNNING,
//	      and export it. `palai up` is idempotent and is normally run against an existing stack, so this
//	      is the common path and it produces ONE clean bring-up with the right values.
//	[4/6] AFTER the bring-up: read GET /v1/deployment and compare desired against effective. On a FIRST
//	      install there was no control plane to read from at [3/6], so this is where a document written
//	      before the machine ever came up is found; a drift here is repaired with one control-plane
//	      recreate and re-verified.
//	      If it STILL disagrees, the command fails with the settings named. A bring-up that quietly left
//	      the machine running something other than what the operator saved is the failure this exists to
//	      make impossible.
//
// WHY NOT READ POSTGRES DIRECTLY. The CLI can (doctor does, for its clock and migration probes) and it
// would save the recreate on a first install. It would also be a second reader of a document whose
// allow-list, value grammar and drift rule all live in the control plane — and the first time the two
// disagreed, the machine would be configured by the one nobody was looking at.

// desiredEnv is the desired document as this CLI needs it: the settings to export, and enough metadata to
// say something useful when they cannot be applied.
type desiredEnv struct {
	revision int64
	settings map[string]string
	// drifted is what the CONTROL PLANE says differs between the document and its own environment. It is
	// taken from the server rather than recomputed here for the reason above: one drift rule, and it is
	// the one the screen shows.
	drifted []string
	// present is false when the deployment has no desired document at all, which is a REAL state and not
	// an error — a machine nobody has configured from the panel runs on its compose file's defaults, and
	// saying so is different from saying the settings are empty.
	present bool
}

// deploymentDesired reads GET /v1/deployment and projects the desired half of it.
//
// A 404 or a body with a null `desired` is "no document", not a failure: an older control plane has no
// such block and a fresh install has no such row, and both must bring up exactly as they always did.
func (c *apiClient) deploymentDesired() (desiredEnv, error) {
	var body struct {
		Settings []struct {
			Name       string `json:"name"`
			Desired    string `json:"desired"`
			DesiredSet bool   `json:"desired_set"`
		} `json:"settings"`
		Desired *struct {
			Revision int64    `json:"revision"`
			Drifted  []string `json:"drifted"`
		} `json:"desired"`
	}
	status, err := c.do(http.MethodGet, "/v1/deployment", nil, &body)
	if err != nil {
		return desiredEnv{}, err
	}
	if status == http.StatusNotFound {
		// A control plane older than this CLI. It has no desired surface, so there is nothing to apply and
		// nothing is wrong.
		return desiredEnv{}, nil
	}
	if status != http.StatusOK {
		return desiredEnv{}, fmt.Errorf("GET /v1/deployment = %d", status)
	}
	if body.Desired == nil {
		return desiredEnv{}, nil
	}
	out := desiredEnv{revision: body.Desired.Revision, settings: map[string]string{}, drifted: body.Desired.Drifted, present: true}
	for _, row := range body.Settings {
		if row.DesiredSet {
			out.settings[row.Name] = row.Desired
		}
	}
	return out, nil
}

// applyDesiredEnv exports the desired settings into THIS process's environment, which is what
// `docker compose` interpolates ${PALAI_X} from and what the native control plane inherits wholesale
// (native.go's nativeEnv merges over os.Environ). It returns the names it set, in order, for the report.
//
// IT OVERWRITES rather than defers to an existing value, and that is the decision this function makes.
// The alternative — leave a variable the operator's shell already exports alone — sounds respectful and is
// the bug: `palai up` run from a shell that once exported PALAI_DISPATCH_WORKERS=0 would silently ignore
// the document forever, and the screen would show a pending bring-up that a bring-up does not clear. The
// document is the machine's configuration; a shell variable is one invocation's. When they disagree the
// machine wins, and the report says which values came from the document so the operator can see it happen.
func applyDesiredEnv(env desiredEnv) ([]string, error) {
	names := make([]string, 0, len(env.settings))
	for name := range env.settings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.Setenv(name, env.settings[name]); err != nil {
			return nil, fmt.Errorf("export the desired value of %s: %w", name, err)
		}
	}
	return names, nil
}

// readDesiredFromRunningStack is step [3/6]'s half: the document as the CURRENTLY RUNNING control plane
// holds it, if one is running.
//
// EVERY FAILURE HERE IS SILENT AND THAT IS DELIBERATE. On a first install there is no stack, no API key
// and no route; on a re-run of a stack that was torn down there is a config and no listener. None of those
// is a problem — the after-the-bring-up check below is what makes the document authoritative, and this
// step exists only to save that check a control-plane recreate in the common case. A refusal here would
// turn "you have never brought this stack up" into a failed command.
func readDesiredFromRunningStack() desiredEnv {
	cfg, p, err := loadConfig()
	if err != nil {
		return desiredEnv{}
	}
	key, err := readTrimmed(p.apiKey)
	if err != nil || key == "" {
		return desiredEnv{}
	}
	api := &apiClient{baseURL: cfg.BaseURL, key: key, http: &http.Client{Timeout: 5 * time.Second}}
	env, err := api.deploymentDesired()
	if err != nil {
		return desiredEnv{}
	}
	return env
}

// errDesiredNotApplied is the refusal `palai up` ends on when the machine is still not running what the
// operator saved. It is a named error so up.go can print it with the rest of its report rather than as a
// bare failure.
var errDesiredNotApplied = errors.New("the desired configuration was not applied")

// verifyDesiredApplied is step [4/6]'s half, and it is the only part of this file that is load-bearing for
// correctness: it re-reads the machine's OWN report of desired-vs-effective and refuses to let the command
// claim success while they differ.
//
// It repairs once — a control-plane recreate, which is what a bring-up that started before the document was
// read needs — and then re-reads. A second disagreement is not repaired, because a second disagreement is
// not a sequencing problem: it means the value did not reach the process, and the most likely cause has a
// name, so the refusal says it.
func verifyDesiredApplied(api *apiClient, recreate func() error) (string, error) {
	env, err := api.deploymentDesired()
	if err != nil {
		return "", fmt.Errorf("read the desired configuration back: %w", err)
	}
	if !env.present {
		return "no desired configuration saved for this machine — it is running on its compose file's own defaults", nil
	}
	if len(env.drifted) == 0 {
		return fmt.Sprintf("revision %d applied — %d setting(s) from the machine's own document", env.revision, len(env.settings)), nil
	}

	// One repair. The stack was brought up before the document could be read (a first install, or a
	// document written while this command was running), so the values are in this process's environment
	// now but not in the container's.
	fmt.Fprintf(os.Stderr, "        desired revision %d is not applied yet (%s) — recreating the control-plane with it\n",
		env.revision, strings.Join(env.drifted, ", "))
	if _, err := applyDesiredEnv(env); err != nil {
		return "", err
	}
	if err := recreate(); err != nil {
		return "", err
	}
	if env, err = api.deploymentDesired(); err != nil {
		return "", fmt.Errorf("read the desired configuration back after the recreate: %w", err)
	}
	if len(env.drifted) == 0 {
		return fmt.Sprintf("revision %d applied after a control-plane recreate — %d setting(s)", env.revision, len(env.settings)), nil
	}
	return "", fmt.Errorf("%w: revision %d still differs from what the control plane is running for %s.\n"+
		"        The most likely cause is that deploy/compose/compose.yaml does not pass one of these to the container: "+
		"a setting the file does not name with ${...} is a setting an exported value cannot reach, whatever this command does. "+
		"apps/control-plane/api's TestEveryWritableSettingIsPassedByTheShippedComposeFile is the guard on that",
		errDesiredNotApplied, env.revision, strings.Join(env.drifted, ", "))
}
