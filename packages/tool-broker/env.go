package toolbroker

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// This file is the ONE place the environment-key rule lives, for the reason redact.go gives about
// redaction: it is a property of the RESULT, not of the container. A key rule written twice — once in
// the host executor and once in the OCI one — is a key rule that diverges the first time one posture
// gains a variable.
//
// It sits in the broker package because both sandbox adaptors already depend on it (RedactSecrets) and
// the broker depends on neither of them.

// envKeyPattern is the shape an environment key must have: an upper-case identifier. Lower-case and
// dotted forms are refused rather than normalised — a key the operator typed is the key the agent's
// shell sees, and a silent transformation would make `printenv` disagree with the console.
var envKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ValidEnvKey reports whether key may name an environment value, or the reason it may not. Two rules,
// and the second is the one worth reading: `PALAI_` is reserved. packages/runner/supervisor.go's
// allowlist is `^PALAI_[A-Z0-9_]+$` and its reservedEnvKeys are the variables the SUPERVISOR sets —
// so a `PALAI_`-prefixed environment key would be indistinguishable, to a reader and eventually to a
// program, from a platform-set variable. The host posture makes this concrete: PALAI_SIMCTL_SET is
// derived per allocation, and an operator able to name it could point a run's simulator device set at
// another run's.
//
// THE COLLISION RULE IS NOT HERE, AND THAT IS DELIBERATE. Which names are already taken depends on the
// posture (PATH/LANG/DEVELOPER_DIR + HOME/TMPDIR/PALAI_SIMCTL_SET on the host, HOME/PATH in the
// container), so a list copied into this function would rot the first time a posture changed. LayerEnv
// takes it from the executor instead.
func ValidEnvKey(key string) error {
	if !envKeyPattern.MatchString(key) {
		return fmt.Errorf("environment key %q must match %s", key, envKeyPattern)
	}
	if strings.HasPrefix(key, "PALAI_") {
		return fmt.Errorf("environment key %q is reserved: the PALAI_ prefix names platform-set variables", key)
	}
	return nil
}

// LayerEnv returns base with extra layered ON TOP, or an error naming the first key it refuses.
//
//   - base is the environment the executor built for itself, in `NAME=VALUE` form.
//   - reserved is every name the posture CLAIMS, whether or not it populated one. It may be nil for a
//     posture whose base is unconditional.
//   - extra is the attempt's own environment values.
//
// NOTHING IN base OR reserved IS OVERWRITTEN: a colliding key is refused and the caller does not run the
// command. So an environment key named PATH cannot shadow the sandbox's PATH.
//
// WHY reserved EXISTS, AND IT IS A MEASUREMENT RATHER THAN A PRECAUTION. This function first checked
// only base, on the reasoning that deriving the taken set from what the executor ACTUALLY built cannot
// rot the way a copied list can. That reasoning was HALF right and the missing half was found by the
// host executor's own test on 2026-07-30: allowedEnv builds its entries with os.LookupEnv, so a name
// the control plane does not itself carry is ABSENT from base — and on a Mac with no exported
// DEVELOPER_DIR, an environment key named DEVELOPER_DIR was accepted and would have selected which
// Xcode the agent's `xcrun` resolved against. A conditionally-populated allow-list makes "present in
// base" and "claimed by the posture" two different sets, and it is the second one that matters.
//
// The refusal is an ERROR rather than a skip. A skipped key would run the command in an environment the
// operator did not describe: a `gh` invocation with no GH_TOKEN succeeds anonymously against a public
// repository, which is a wrong answer that looks like a right one.
//
// The result is sorted by key so two executions of the same attempt build the same environment bytes;
// Go's map order would otherwise make an environment dump non-deterministic for no reason.
func LayerEnv(base, reserved []string, extra map[string]string) ([]string, error) {
	if len(extra) == 0 {
		return base, nil
	}
	taken := make(map[string]bool, len(base)+len(reserved))
	for _, entry := range base {
		if name, _, ok := strings.Cut(entry, "="); ok {
			taken[name] = true
		}
	}
	for _, name := range reserved {
		taken[name] = true
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(base)+len(keys))
	out = append(out, base...)
	for _, key := range keys {
		if err := ValidEnvKey(key); err != nil {
			return nil, err
		}
		if taken[key] {
			return nil, fmt.Errorf("environment key %q is reserved by the sandbox's own environment and must not be shadowed", key)
		}
		out = append(out, key+"="+extra[key])
	}
	return out, nil
}

// EnvValueList is the attempt's environment VALUES, for RedactValues. It exists so a caller cannot
// accidentally hand RedactValues the KEYS (a map's range yields keys, and `for k := range m` is one
// character away from `for _, v := range m`) — a redactor fed key names would mask nothing and report
// success.
func EnvValueList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, v := range env {
		out = append(out, v)
	}
	return out
}
