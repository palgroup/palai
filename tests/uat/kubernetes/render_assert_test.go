// Package kubernetes render-asserts the E15 T3 restricted Helm chart. These are DETERMINISTIC gates
// that need no cluster: they run `helm lint`, render the chart with `helm template`, assert the
// security/policy posture over the rendered objects, and schema-validate them with `kubeconform`.
//
// SCOPE (plan §T3 honest ceiling): this proves lint + render + policy CORRECTNESS at the render/schema
// level. It does NOT prove real managed-K8s admission, and it does NOT prove NetworkPolicy ENFORCEMENT —
// kind's default CNI (kindnet) does not enforce NetworkPolicy; enforcement needs a policy-enforcing CNI
// on a real cluster (operator leg §6). The live install is the separate kind smoke (kind-smoke.sh).
package kubernetes

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// object is one rendered Kubernetes manifest, decoded loosely so the asserts can walk arbitrary paths.
type object struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   map[string]any `yaml:"metadata"`
	Spec       map[string]any `yaml:"spec"`
	Raw        map[string]any `yaml:"-"`
}

// renderChart runs `helm template` with the shared render values and decodes every emitted document,
// INCLUDING hook objects (helm template renders hooks), which is how the migration Job is asserted.
func renderChart(t *testing.T) []object {
	t.Helper()
	requireBin(t, "helm")
	root := repoRoot(t)
	chart := filepath.Join(root, "deploy", "helm", "palai")
	values := filepath.Join(root, "tests", "uat", "kubernetes", "testdata", "render-values.yaml")

	cmd := exec.Command("helm", "template", "palai", chart, "--namespace", "palai", "--values", values)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, errb.String())
	}
	return decodeDocs(t, out.Bytes())
}

func decodeDocs(t *testing.T, data []byte) []object {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var objs []object
	for {
		var raw map[string]any
		err := dec.Decode(&raw)
		if err != nil {
			break
		}
		if len(raw) == 0 {
			continue
		}
		o := object{Raw: raw}
		o.Kind, _ = raw["kind"].(string)
		o.APIVersion, _ = raw["apiVersion"].(string)
		o.Metadata, _ = raw["metadata"].(map[string]any)
		o.Spec, _ = raw["spec"].(map[string]any)
		objs = append(objs, o)
	}
	if len(objs) == 0 {
		t.Fatal("helm template produced no objects")
	}
	return objs
}

// TestHelmLint runs `helm lint` — the chart must lint clean (a schema/template error fails it).
func TestHelmLint(t *testing.T) {
	requireBin(t, "helm")
	root := repoRoot(t)
	cmd := exec.Command("helm", "lint", filepath.Join(root, "deploy", "helm", "palai"),
		"--values", filepath.Join(root, "tests", "uat", "kubernetes", "testdata", "render-values.yaml"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, out)
	}
}

// TestNoClusterRole is the load-bearing security assert (plan §45.3): the chart grants NO cluster-wide
// permission — there is ZERO ClusterRole/ClusterRoleBinding. It checks the object KIND, not the source
// substring (the rbac.yaml comment mentions ClusterRole deliberately), and confirms the namespace-scoped
// Role/RoleBinding IS present.
func TestNoClusterRole(t *testing.T) {
	objs := renderChart(t)
	for _, o := range objs {
		if o.Kind == "ClusterRole" || o.Kind == "ClusterRoleBinding" {
			t.Fatalf("chart rendered a %s — no ongoing cluster-admin is allowed (plan §45.3)", o.Kind)
		}
	}
	if !hasKind(objs, "Role") {
		t.Fatal("expected a namespace-scoped Role")
	}
	if !hasKind(objs, "RoleBinding") {
		t.Fatal("expected a namespace-scoped RoleBinding")
	}
}

// TestControlPlaneSecurityContextRestricted pins the Pod-Security "restricted" posture on the
// control-plane Deployment: runAsNonRoot, no privilege escalation, all capabilities dropped, the
// RuntimeDefault seccomp profile, and a read-only root filesystem.
func TestControlPlaneSecurityContextRestricted(t *testing.T) {
	objs := renderChart(t)
	dep := requireObject(t, objs, "Deployment", "palai")
	assertRestrictedPodSpec(t, podSpec(t, dep))
}

// TestMigrationJobSecurityContextRestricted pins the SAME restricted posture on the migration Job — a
// hook Pod is subject to Pod Security admission too.
func TestMigrationJobSecurityContextRestricted(t *testing.T) {
	objs := renderChart(t)
	job := requireObject(t, objs, "Job", "palai-migrate")
	assertRestrictedPodSpec(t, podSpec(t, job))
}

// TestMigrationJobIsPreInstallHook pins that the migration Job is a pre-install/pre-upgrade Helm hook
// invoking the binary's --migrate-and-exit mode — the single ordered migrator that runs BEFORE the
// control-plane Deployment rolls.
func TestMigrationJobIsPreInstallHook(t *testing.T) {
	objs := renderChart(t)
	job := requireObject(t, objs, "Job", "palai-migrate")
	anns, _ := job.Metadata["annotations"].(map[string]any)
	hook, _ := anns["helm.sh/hook"].(string)
	if !strings.Contains(hook, "pre-install") || !strings.Contains(hook, "pre-upgrade") {
		t.Fatalf("migration Job hook = %q, want pre-install,pre-upgrade", hook)
	}
	spec := podSpec(t, job)
	c := containers(t, spec)[0]
	cmdList, _ := c["command"].([]any)
	joined := joinAny(cmdList)
	if !strings.Contains(joined, "--migrate-and-exit") {
		t.Fatalf("migration Job command = %q, want it to include --migrate-and-exit", joined)
	}
}

// TestNetworkPolicyDefaultDeny pins that a default-deny (podSelector:{}) NetworkPolicy covering BOTH
// Ingress and Egress is present alongside the scoped allow.
func TestNetworkPolicyDefaultDeny(t *testing.T) {
	objs := renderChart(t)
	var deny bool
	for _, o := range objs {
		if o.Kind != "NetworkPolicy" {
			continue
		}
		sel, _ := o.Spec["podSelector"].(map[string]any)
		types := strings.Join(stringsOf(o.Spec["policyTypes"]), ",")
		if len(sel) == 0 && strings.Contains(types, "Ingress") && strings.Contains(types, "Egress") {
			deny = true
		}
	}
	if !deny {
		t.Fatal("expected a default-deny NetworkPolicy (empty podSelector, Ingress+Egress)")
	}
}

// TestEveryPodTemplateSelectedByEgressAllow closes the MUST-FIX-2 blind spot permanently: on an enforcing
// CNI the default-deny cuts ALL egress, so every pod the chart schedules — the control-plane Deployment AND
// the pre-install migration Job — must be selected by an egress-ALLOW NetworkPolicy (one with a non-empty
// egress rule) or it loses DNS + database egress and (for the hook Job) deadlocks. An allow policy whose
// podSelector pins a component label the Job pod lacks regresses this; the assert fails on exactly that.
func TestEveryPodTemplateSelectedByEgressAllow(t *testing.T) {
	objs := renderChart(t)

	// Selectors of every egress-ALLOW NetworkPolicy (non-empty spec.egress); default-deny grants nothing.
	var allowSelectors []map[string]string
	for _, o := range objs {
		if o.Kind != "NetworkPolicy" {
			continue
		}
		if egress, _ := o.Spec["egress"].([]any); len(egress) == 0 {
			continue
		}
		sel, _ := o.Spec["podSelector"].(map[string]any)
		ml, _ := sel["matchLabels"].(map[string]any)
		allowSelectors = append(allowSelectors, stringMap(ml))
	}
	if len(allowSelectors) == 0 {
		t.Fatal("no egress-allow NetworkPolicy rendered — every pod would be cut off on an enforcing CNI")
	}

	// Every pod-scheduling object's template labels must be selected by at least one egress-allow policy.
	var checked int
	for _, o := range objs {
		labels := podTemplateLabels(o)
		if len(labels) == 0 {
			continue // not a pod-scheduling object
		}
		checked++
		if !anySelectorMatches(allowSelectors, labels) {
			t.Fatalf("%s/%s (pod-template labels %v) is not selected by any egress-allow NetworkPolicy — on an enforcing CNI it loses DNS + database egress", o.Kind, name(o), labels)
		}
	}
	if checked < 2 {
		t.Fatalf("expected at least the control-plane Deployment + migration Job pod templates, checked %d", checked)
	}
}

// podTemplateLabels extracts spec.template.metadata.labels from a pod-scheduling object (Deployment/Job/…).
func podTemplateLabels(o object) map[string]string {
	tmpl, _ := o.Spec["template"].(map[string]any)
	meta, _ := tmpl["metadata"].(map[string]any)
	ml, _ := meta["labels"].(map[string]any)
	return stringMap(ml)
}

// stringMap coerces a decoded map[string]any of string values into map[string]string.
func stringMap(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// anySelectorMatches reports whether any selector's matchLabels is a subset of the pod's labels.
func anySelectorMatches(selectors []map[string]string, labels map[string]string) bool {
	for _, sel := range selectors {
		subset := true
		for k, v := range sel {
			if labels[k] != v {
				subset = false
				break
			}
		}
		if subset {
			return true
		}
	}
	return false
}

// TestMigrationJobHasNoServiceAccountName is the MUST-FIX-1 regression guard: the pre-install hook Job must
// NOT name the chart's ServiceAccount, which does not exist yet when Helm runs pre-install hooks — naming it
// makes the SA admission plugin reject the pod and deadlocks every fresh `helm install`. The always-present
// `default` SA is correct (the hook needs no Kubernetes API). A future "consolidate the SA" cleanup that
// re-adds serviceAccountName would reintroduce the deadlock; this fails at render time instead of only under
// the expensive kind smoke.
func TestMigrationJobHasNoServiceAccountName(t *testing.T) {
	objs := renderChart(t)
	job := requireObject(t, objs, "Job", "palai-migrate")
	tmpl, _ := job.Spec["template"].(map[string]any)
	spec, _ := tmpl["spec"].(map[string]any)
	if sa, ok := spec["serviceAccountName"]; ok {
		t.Fatalf("migration Job names serviceAccountName=%v — a pre-install hook runs BEFORE the chart's SA exists, so this deadlocks every fresh install; remove it (the default SA is correct)", sa)
	}
}

// TestPodDisruptionBudgetPresent pins the PDB is rendered.
func TestPodDisruptionBudgetPresent(t *testing.T) {
	objs := renderChart(t)
	if !hasKind(objs, "PodDisruptionBudget") {
		t.Fatal("expected a PodDisruptionBudget")
	}
}

// TestNoInClusterDatabase pins the chart runs NO in-cluster DB (plan §2 — external PG/S3 are config
// only): there is no StatefulSet, and the ONLY Deployment is the control-plane. The DB URL is injected
// from a Secret via secretKeyRef, never as a literal env value.
func TestNoInClusterDatabase(t *testing.T) {
	objs := renderChart(t)
	if hasKind(objs, "StatefulSet") {
		t.Fatal("chart rendered a StatefulSet — it must run NO in-cluster database (external PG/S3 only)")
	}
	deps := 0
	for _, o := range objs {
		if o.Kind == "Deployment" {
			deps++
		}
	}
	if deps != 1 {
		t.Fatalf("expected exactly 1 Deployment (the control-plane), got %d", deps)
	}
	dep := requireObject(t, objs, "Deployment", "palai")
	c := containers(t, podSpec(t, dep))[0]
	if !dbURLFromSecret(c) {
		t.Fatal("PALAI_DATABASE_URL must be injected from a Secret (secretKeyRef), never a literal value")
	}
}

// TestReplicaCountOne pins the no-HA-claim default: replicas=1 (plan §45.2).
func TestReplicaCountOne(t *testing.T) {
	objs := renderChart(t)
	dep := requireObject(t, objs, "Deployment", "palai")
	if r, ok := dep.Spec["replicas"]; !ok || toInt(r) != 1 {
		t.Fatalf("Deployment replicas = %v, want 1 (no HA claim)", dep.Spec["replicas"])
	}
}

// TestKubeconform schema-validates the whole render with kubeconform (strict). Skips when the binary is
// absent so the render/policy asserts above still run in a minimal environment.
func TestKubeconform(t *testing.T) {
	if _, err := exec.LookPath("kubeconform"); err != nil {
		t.Skip("kubeconform not installed (brew install kubeconform) — schema validation skipped")
	}
	requireBin(t, "helm")
	root := repoRoot(t)
	tmpl := exec.Command("helm", "template", "palai", filepath.Join(root, "deploy", "helm", "palai"),
		"--namespace", "palai", "--values", filepath.Join(root, "tests", "uat", "kubernetes", "testdata", "render-values.yaml"))
	rendered, err := tmpl.Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	kc := exec.Command("kubeconform", "-strict", "-summary", "-kubernetes-version", "1.29.0")
	kc.Stdin = bytes.NewReader(rendered)
	out, err := kc.CombinedOutput()
	if err != nil {
		t.Fatalf("kubeconform found invalid manifests:\n%s", out)
	}
}

// --- helpers ---

func assertRestrictedPodSpec(t *testing.T, spec map[string]any) {
	t.Helper()
	podSec, _ := spec["securityContext"].(map[string]any)
	if b, _ := podSec["runAsNonRoot"].(bool); !b {
		t.Error("pod securityContext.runAsNonRoot must be true")
	}
	seccomp, _ := podSec["seccompProfile"].(map[string]any)
	if tp, _ := seccomp["type"].(string); tp != "RuntimeDefault" {
		t.Errorf("pod seccompProfile.type = %q, want RuntimeDefault", tp)
	}
	c := containers(t, spec)[0]
	csec, _ := c["securityContext"].(map[string]any)
	if b, _ := csec["runAsNonRoot"].(bool); !b {
		t.Error("container runAsNonRoot must be true")
	}
	if b, ok := csec["allowPrivilegeEscalation"].(bool); !ok || b {
		t.Error("container allowPrivilegeEscalation must be false")
	}
	if b, _ := csec["readOnlyRootFilesystem"].(bool); !b {
		t.Error("container readOnlyRootFilesystem must be true")
	}
	caps, _ := csec["capabilities"].(map[string]any)
	drop := stringsOf(caps["drop"])
	if !contains(drop, "ALL") {
		t.Errorf("container capabilities.drop = %v, want it to contain ALL", drop)
	}
}

func podSpec(t *testing.T, o object) map[string]any {
	t.Helper()
	tmpl, _ := o.Spec["template"].(map[string]any)
	spec, _ := tmpl["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("%s %s has no pod template spec", o.Kind, name(o))
	}
	return spec
}

func containers(t *testing.T, spec map[string]any) []map[string]any {
	t.Helper()
	raw, _ := spec["containers"].([]any)
	if len(raw) == 0 {
		t.Fatal("pod spec has no containers")
	}
	var out []map[string]any
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func dbURLFromSecret(c map[string]any) bool {
	env, _ := c["env"].([]any)
	for _, e := range env {
		m, _ := e.(map[string]any)
		if m["name"] != "PALAI_DATABASE_URL" {
			continue
		}
		if _, literal := m["value"]; literal {
			return false // a literal value is a leak
		}
		vf, _ := m["valueFrom"].(map[string]any)
		_, ok := vf["secretKeyRef"].(map[string]any)
		return ok
	}
	return false
}

func requireObject(t *testing.T, objs []object, kind, objName string) object {
	t.Helper()
	for _, o := range objs {
		if o.Kind == kind && name(o) == objName {
			return o
		}
	}
	t.Fatalf("no %s named %q in render", kind, objName)
	return object{}
}

func hasKind(objs []object, kind string) bool {
	for _, o := range objs {
		if o.Kind == kind {
			return true
		}
	}
	return false
}

func name(o object) string {
	n, _ := o.Metadata["name"].(string)
	return n
}

func stringsOf(v any) []string {
	raw, _ := v.([]any)
	var out []string
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func joinAny(v []any) string {
	var parts []string
	for _, e := range v {
		if s, ok := e.(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return -1
}

func requireBin(t *testing.T, bin string) {
	t.Helper()
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not installed — render-assert skipped", bin)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
