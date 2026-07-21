package automation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The bounded declarative input-mapping language (spec §20.2.2, E11 Task 2). A mapping transforms a
// source event payload into a canonical action input; dedupe-key and correlation-key expressions reuse
// the SAME language (no second language). Its power is deliberately bounded to a CLOSED set of pure
// verbs — select (with a dotted path + optional default), const, when (equality conditional), and secret
// (an allowlisted SecretRef HANDLE, never plaintext). There is NO verb for network, filesystem, or
// process access: those effects are UNEXPRESSIBLE (structural — the compiler only knows the safe verbs
// and rejects everything else), not merely denied by a blacklist. That is what stops a tenant-authored
// mapping from becoming an SSRF / LFI / RCE gadget.

var (
	// ErrMappingVerb is returned when a rule carries an unrecognized verb (a fetch/file/exec escape is
	// structurally undecodable) or a malformed verb shape.
	ErrMappingVerb = errors.New("automation: mapping rule carries an unsupported or malformed verb")
	// ErrSecretNotAllowed is returned when a secret verb names a SecretRef outside the trigger allowlist.
	ErrSecretNotAllowed = errors.New("automation: mapping secret ref is not in the trigger allowlist")
	// ErrMappingSchema is returned when the mapped canonical input violates the declared output schema
	// (a required field is absent or null). It is the AUT-003 typed failure: a failed delivery, no run.
	ErrMappingSchema = errors.New("automation: mapped input violates the output schema")
)

// knownVerbKeys is the closed whitelist of keys a single rule object may carry. Any other key (fetch,
// file, exec, eval, ...) makes the rule undecodable — the structural bound, not a blacklist.
var knownVerbKeys = map[string]bool{"select": true, "const": true, "when": true, "secret": true, "default": true}

// rule is one compiled mapping expression. Exactly one primary verb is set (kind names it); default is
// an optional select fallback.
type rule struct {
	kind     string // "select" | "const" | "when" | "secret"
	path     []string
	constVal json.RawMessage
	secret   string
	hasDflt  bool
	dflt     json.RawMessage
	// when (equality conditional):
	condPath []string
	equals   json.RawMessage
	then     *rule
	els      *rule
}

// Mapping is a compiled input-mapping program: an ordered set of output fields plus the declared
// required-field schema. Apply is pure — it depends only on the source payload.
type Mapping struct {
	fields   []mappedField
	required []string
}

type mappedField struct {
	name string
	r    rule
}

// Expr is a compiled single-rule expression in the same language, used for the dedupe-key and
// correlation-key. EvalString stringifies its scalar result.
type Expr struct{ r rule }

// CompileMapping strictly decodes an input_mapping document ({"fields":{name:rule,...},"required":[...]})
// into a Mapping, rejecting any unknown verb or out-of-allowlist secret at compile time. An empty
// document ({}) compiles to an identity-empty mapping (no fields, no schema).
func CompileMapping(raw []byte, allowedSecrets []string) (Mapping, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Mapping{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc struct {
		Fields   map[string]json.RawMessage `json:"fields"`
		Required []string                   `json:"required"`
	}
	if err := dec.Decode(&doc); err != nil {
		return Mapping{}, fmt.Errorf("%w: %v", ErrMappingVerb, err)
	}
	allow := secretSet(allowedSecrets)
	// Compile fields in a stable (sorted) order so a preview is deterministic.
	names := make([]string, 0, len(doc.Fields))
	for name := range doc.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	m := Mapping{required: doc.Required}
	for _, name := range names {
		r, err := compileRule(doc.Fields[name], allow)
		if err != nil {
			return Mapping{}, fmt.Errorf("field %q: %w", name, err)
		}
		m.fields = append(m.fields, mappedField{name: name, r: r})
	}
	return m, nil
}

// CompileExpr compiles a single-rule expression (the dedupe/correlation key language). An empty string
// compiles to a nil expression whose EvalString returns "".
func CompileExpr(raw string, allowedSecrets []string) (Expr, error) {
	if strings.TrimSpace(raw) == "" {
		return Expr{}, nil
	}
	r, err := compileRule(json.RawMessage(raw), secretSet(allowedSecrets))
	if err != nil {
		return Expr{}, err
	}
	return Expr{r: r}, nil
}

// Apply evaluates the mapping against a source payload, producing the canonical action input JSON, or a
// typed schema error when a required output field is absent or null.
func (m Mapping) Apply(payload map[string]any) ([]byte, error) {
	out := map[string]any{}
	for _, f := range m.fields {
		v, ok := evalRule(f.r, payload)
		if ok {
			out[f.name] = v
		}
	}
	for _, req := range m.required {
		if v, ok := out[req]; !ok || v == nil {
			return nil, fmt.Errorf("%w: required field %q is absent", ErrMappingSchema, req)
		}
	}
	return json.Marshal(out)
}

// EvalString evaluates the expression against a source payload and stringifies its scalar result (""
// for a nil expression, an absent path, or a null). A length bound is applied by the caller (the dedupe/
// correlation key is SHA-256'd and bounded there).
func (e Expr) EvalString(payload map[string]any) (string, error) {
	if e.r.kind == "" {
		return "", nil
	}
	v, ok := evalRule(e.r, payload)
	if !ok || v == nil {
		return "", nil
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case map[string]any:
		// A secret handle must never become part of a dedupe/correlation key (it would leak the ref into
		// a queryable key and defeat the point of a handle). Reject it.
		if _, isSecret := t["secret_ref"]; isSecret {
			return "", fmt.Errorf("%w: a secret ref cannot be used as a key", ErrMappingVerb)
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// compileRule decodes one rule object, enforcing the closed verb whitelist + the secret allowlist.
func compileRule(raw json.RawMessage, allow map[string]bool) (rule, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return rule{}, fmt.Errorf("%w: rule is not an object: %v", ErrMappingVerb, err)
	}
	if len(keys) == 0 {
		return rule{}, fmt.Errorf("%w: empty rule", ErrMappingVerb)
	}
	// Structural bound: every key must be a known verb. An unknown key (fetch/file/exec/...) is rejected
	// here, so an escape is unexpressible rather than blacklisted.
	for k := range keys {
		if !knownVerbKeys[k] {
			return rule{}, fmt.Errorf("%w: unknown verb %q", ErrMappingVerb, k)
		}
	}
	switch {
	case keys["select"] != nil:
		var path string
		if err := json.Unmarshal(keys["select"], &path); err != nil || path == "" {
			return rule{}, fmt.Errorf("%w: select needs a non-empty path", ErrMappingVerb)
		}
		r := rule{kind: "select", path: splitPath(path)}
		if d, ok := keys["default"]; ok {
			r.hasDflt, r.dflt = true, d
		}
		return r, nil
	case keys["const"] != nil:
		return rule{kind: "const", constVal: keys["const"]}, nil
	case keys["secret"] != nil:
		var name string
		if err := json.Unmarshal(keys["secret"], &name); err != nil || name == "" {
			return rule{}, fmt.Errorf("%w: secret needs a non-empty ref name", ErrMappingVerb)
		}
		if !allow[name] {
			return rule{}, fmt.Errorf("%w: %q", ErrSecretNotAllowed, name)
		}
		return rule{kind: "secret", secret: name}, nil
	case keys["when"] != nil:
		return compileWhen(keys["when"], allow)
	default:
		return rule{}, fmt.Errorf("%w: rule names no primary verb", ErrMappingVerb)
	}
}

// compileWhen decodes an equality conditional: {"path":..,"equals":..,"then":rule,"else":rule}.
func compileWhen(raw json.RawMessage, allow map[string]bool) (rule, error) {
	var w struct {
		Path   string          `json:"path"`
		Equals json.RawMessage `json:"equals"`
		Then   json.RawMessage `json:"then"`
		Else   json.RawMessage `json:"else"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return rule{}, fmt.Errorf("%w: malformed when: %v", ErrMappingVerb, err)
	}
	if w.Path == "" || w.Then == nil || w.Else == nil {
		return rule{}, fmt.Errorf("%w: when needs path, then, and else", ErrMappingVerb)
	}
	then, err := compileRule(w.Then, allow)
	if err != nil {
		return rule{}, err
	}
	els, err := compileRule(w.Else, allow)
	if err != nil {
		return rule{}, err
	}
	return rule{kind: "when", condPath: splitPath(w.Path), equals: w.Equals, then: &then, els: &els}, nil
}

// evalRule evaluates a compiled rule against the payload. ok=false marks an absent select with no
// default (the field is then omitted from the output).
func evalRule(r rule, payload map[string]any) (any, bool) {
	switch r.kind {
	case "select":
		if v, ok := lookupPath(payload, r.path); ok {
			return v, true
		}
		if r.hasDflt {
			return decodeAny(r.dflt), true
		}
		return nil, false
	case "const":
		return decodeAny(r.constVal), true
	case "secret":
		// A redacted handle, never plaintext — the pipeline/resolver redeems it later.
		return map[string]any{"secret_ref": r.secret}, true
	case "when":
		v, _ := lookupPath(payload, r.condPath)
		if valuesEqual(v, decodeAny(r.equals)) {
			return evalRule(*r.then, payload)
		}
		return evalRule(*r.els, payload)
	default:
		return nil, false
	}
}

// lookupPath walks a dotted path into a decoded JSON payload.
func lookupPath(payload map[string]any, path []string) (any, bool) {
	var cur any = payload
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[seg]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func splitPath(p string) []string { return strings.Split(p, ".") }

func decodeAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

// valuesEqual compares two decoded JSON scalars/objects by their canonical JSON encoding.
func valuesEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

func secretSet(allowed []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range allowed {
		out[s] = true
	}
	return out
}
