// Package outputcontract owns the run's OUTPUT CONTRACT: the `output` field of a response-create
// request (spec §8.2, §22.7), the safety checks that decide whether a schema may be accepted at all,
// and the validation of a finished run's answer against it.
//
// WHY THIS IS ONE PACKAGE. The contract is read in three places that must agree exactly — the API
// (which accepts or refuses it), the model dispatcher (which turns it into a provider constraint),
// and finalize (which checks the answer before the run is called completed). Before this package the
// field was read in NONE of them: `output` was declared in the published schema, decoded into
// contracts.ResponseCreateRequest.Output, folded into the idempotency hash, and then dropped, so a
// caller who demanded a schema got prose and a `completed` status. Splitting the contract across the
// three call sites is how that happens twice.
//
// THE FAIL-CLOSED RULE, AND IT IS THE WHOLE DESIGN. Validate() is not a general JSON Schema 2020-12
// implementation and this package does not pretend otherwise. Instead Check() walks a submitted
// schema at ADMISSION and REFUSES every construct Validate() cannot fully enforce. So the pair is
// sound by construction: a schema that was accepted is a schema that is checked exactly, and a schema
// carrying a keyword we would have had to ignore is a 400 the caller can read — never a silently
// weaker guarantee. Under-validating is the dangerous direction (it returns "valid" for output that
// is not), and this is how it is made unreachable. Widening support means widening BOTH functions,
// and a keyword added to one without the other fails Check's own test.
package outputcontract

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// FormatText is the default: free-form output, no schema, no validation. It is what every request
// that names no format has always meant, and naming it explicitly changes nothing.
const FormatText = "text"

// FormatJSONSchema demands that the final output parse as JSON and validate against Schema.
const FormatJSONSchema = "json_schema"

// maxSchemaDepth bounds nesting. Spec §22.7 forbids "unbounded recursive expansion"; a JSON document
// cannot express true recursion without $ref (which Check refuses outright), so depth is the residual
// concern — a deeply nested literal schema is cheap to send and expensive to walk.
const maxSchemaDepth = 12

// maxSchemaProperties bounds total property count across the whole document, for the same reason.
const maxSchemaProperties = 512

// Contract is a run's resolved output contract. The zero value means FormatText — no schema, no
// validation — which is what a request without an `output` field resolves to.
type Contract struct {
	Format string
	Name   string
	Schema map[string]any
	Strict bool
}

// Demanded reports whether this contract obliges the run to produce schema-valid JSON.
func (c Contract) Demanded() bool { return c.Format == FormatJSONSchema && len(c.Schema) > 0 }

// ErrInvalid marks a request-shaped refusal: the caller's `output` object is malformed or names
// something this server will not enforce. It renders as a 400, never a 5xx — the request is wrong,
// the server is fine.
var ErrInvalid = errors.New("invalid output contract")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Parse resolves the `output` object of a create request into a Contract, refusing anything it will
// not enforce. A nil/absent map is the text default.
//
// It takes the DECODED map rather than raw bytes because `output` is a declared field of the
// published schema and the generated contract already carries it — unlike `delegations`, which is
// load-bearing, undeclared, and therefore has to be re-probed from the raw body.
func Parse(raw map[string]any) (Contract, error) {
	if len(raw) == 0 {
		return Contract{Format: FormatText}, nil
	}
	// additionalProperties:false on the published `output` object, enforced here too so the refusal
	// exists even for a caller who never validated against the schema. A misspelled key is the exact
	// failure this whole change is about: it must not be silently dropped.
	for _, key := range sortedKeys(raw) {
		switch key {
		case "format", "name", "schema", "strict":
		default:
			return Contract{}, invalid("unknown field %q in output (accepted: format, name, schema, strict)", key)
		}
	}

	c := Contract{Format: FormatText, Strict: true}
	if v, present := raw["format"]; present {
		s, ok := v.(string)
		if !ok {
			return Contract{}, invalid("output.format must be a string, got %T", v)
		}
		switch s {
		case FormatText, FormatJSONSchema:
			c.Format = s
		default:
			// Before 2026-08-01 this value was accepted and dropped. Refusing it is the one
			// behaviour change in this field, and it is the correct direction: a caller who asked
			// for something we do not implement now learns it, instead of receiving prose that
			// silently is not what they asked for.
			return Contract{}, invalid("output.format %q is not supported (accepted: %q, %q)", s, FormatText, FormatJSONSchema)
		}
	}
	if v, present := raw["strict"]; present {
		b, ok := v.(bool)
		if !ok {
			return Contract{}, invalid("output.strict must be a boolean, got %T", v)
		}
		c.Strict = b
	}
	if v, present := raw["name"]; present {
		s, ok := v.(string)
		if !ok {
			return Contract{}, invalid("output.name must be a string, got %T", v)
		}
		if len(s) > 64 {
			return Contract{}, invalid("output.name is %d characters, the limit is 64", len(s))
		}
		c.Name = s
	}
	if v, present := raw["schema"]; present {
		m, ok := v.(map[string]any)
		if !ok {
			return Contract{}, invalid("output.schema must be an object, got %T", v)
		}
		c.Schema = m
	}

	if c.Format == FormatJSONSchema {
		if len(c.Schema) == 0 {
			return Contract{}, invalid("output.format is %q but no output.schema was given", FormatJSONSchema)
		}
		if err := Check(c.Schema); err != nil {
			return Contract{}, err
		}
		if c.Name == "" {
			c.Name = "output"
		}
	} else if len(c.Schema) > 0 {
		// A schema under format:"text" is very likely a caller who believes it is being enforced.
		// It is not, and saying so is cheaper than a silently unvalidated run.
		return Contract{}, invalid("output.schema was given but output.format is %q; set format to %q to have it enforced", c.Format, FormatJSONSchema)
	}
	return c, nil
}

// Check reports the first construct in schema that this server will not enforce. Everything it
// accepts, Validate checks exactly; that equivalence is the package's contract and its test asserts
// it keyword by keyword.
func Check(schema map[string]any) error {
	var properties int
	return check(schema, "", 0, &properties)
}

// supportedKeywords are the keywords Validate actually implements. A keyword outside this set is
// refused rather than ignored: ignoring it would silently weaken the caller's contract, which is the
// failure mode this package exists to prevent.
var supportedKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"items": true, "enum": true, "description": true, "title": true,
	"minimum": true, "maximum": true, "minLength": true, "maxLength": true,
	"minItems": true, "maxItems": true,
}

var supportedTypes = map[string]bool{
	"object": true, "array": true, "string": true, "integer": true,
	"number": true, "boolean": true, "null": true,
}

func check(schema map[string]any, path string, depth int, properties *int) error {
	if depth > maxSchemaDepth {
		return invalid("schema nests deeper than %d levels at %s", maxSchemaDepth, at(path))
	}
	for _, key := range sortedKeys(schema) {
		if key == "$ref" || key == "$dynamicRef" {
			// Spec §22.7: no remote references. A local $ref would also be a recursion vector, and
			// nothing needs one to state an output shape.
			return invalid("schema uses %q at %s; references are not accepted in an output schema", key, at(path))
		}
		if strings.HasPrefix(key, "$") {
			return invalid("schema uses %q at %s; %s keywords are not accepted in an output schema", key, at(path), "$-prefixed")
		}
		if !supportedKeywords[key] {
			return invalid("schema uses %q at %s, which this server does not enforce; "+
				"remove it or express the constraint with a supported keyword (%s)",
				key, at(path), strings.Join(sortedKeys(toAnyMap(supportedKeywords)), ", "))
		}
	}

	typ, _ := schema["type"].(string)
	if _, present := schema["type"]; !present {
		return invalid("schema at %s declares no %q; an untyped schema constrains nothing and would validate any answer", at(path), "type")
	}
	if typ == "" {
		// A type ARRAY (union) is legal JSON Schema and Validate does not implement it.
		return invalid("schema at %s declares a non-string %q; a union type is not accepted", at(path), "type")
	}
	if !supportedTypes[typ] {
		return invalid("schema at %s declares type %q (accepted: %s)", at(path), typ,
			strings.Join(sortedKeys(toAnyMap(supportedTypes)), ", "))
	}

	switch typ {
	case "object":
		props, _ := schema["properties"].(map[string]any)
		if _, present := schema["properties"]; present && props == nil {
			return invalid("schema at %s has a non-object %q", at(path), "properties")
		}
		*properties += len(props)
		if *properties > maxSchemaProperties {
			return invalid("schema declares more than %d properties in total", maxSchemaProperties)
		}
		for _, name := range sortedKeys(props) {
			sub, ok := props[name].(map[string]any)
			if !ok {
				return invalid("schema at %s has a non-object subschema", at(join(path, name)))
			}
			if err := check(sub, join(path, name), depth+1, properties); err != nil {
				return err
			}
		}
		for _, name := range toStrings(schema["required"]) {
			if _, defined := props[name]; !defined {
				return invalid("schema at %s requires property %q which it does not define", at(path), name)
			}
		}
	case "array":
		items, ok := schema["items"].(map[string]any)
		if !ok {
			return invalid("schema at %s is an array without an object %q subschema", at(path), "items")
		}
		if err := check(items, join(path, "[]"), depth+1, properties); err != nil {
			return err
		}
	}
	return nil
}

// Validate reports the first way value fails schema. value is a decoded JSON document.
func Validate(schema map[string]any, value any) error {
	return validate(schema, value, "")
}

func validate(schema map[string]any, value any, path string) error {
	typ, _ := schema["type"].(string)

	if raw, present := schema["enum"]; present {
		if !enumContains(raw, value) {
			return fmt.Errorf("%s is %s, which is not one of the permitted values %s", at(path), render(value), render(raw))
		}
	}

	switch typ {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s should be an object, got %s", at(path), kind(value))
		}
		props, _ := schema["properties"].(map[string]any)
		for _, name := range toStrings(schema["required"]) {
			if _, present := obj[name]; !present {
				return fmt.Errorf("%s is missing required property %q", at(path), name)
			}
		}
		if allow, ok := schema["additionalProperties"].(bool); ok && !allow {
			for _, key := range sortedKeys(obj) {
				if _, defined := props[key]; !defined {
					return fmt.Errorf("%s has property %q, which the schema does not permit", at(path), key)
				}
			}
		}
		for _, name := range sortedKeys(props) {
			raw, present := obj[name]
			if !present {
				continue
			}
			sub, ok := props[name].(map[string]any)
			if !ok {
				continue
			}
			if err := validate(sub, raw, join(path, name)); err != nil {
				return err
			}
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s should be an array, got %s", at(path), kind(value))
		}
		if err := boundInt(schema, "minItems", "maxItems", len(arr), at(path)+" length"); err != nil {
			return err
		}
		items, ok := schema["items"].(map[string]any)
		if !ok {
			return nil
		}
		for i, elem := range arr {
			if err := validate(items, elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s should be a string, got %s", at(path), kind(value))
		}
		if err := boundInt(schema, "minLength", "maxLength", len([]rune(s)), at(path)+" length"); err != nil {
			return err
		}
	case "integer":
		f, ok := asNumber(value)
		if !ok || f != math.Trunc(f) {
			return fmt.Errorf("%s should be an integer, got %s", at(path), kind(value))
		}
		if err := boundNumber(schema, f, at(path)); err != nil {
			return err
		}
	case "number":
		f, ok := asNumber(value)
		if !ok {
			return fmt.Errorf("%s should be a number, got %s", at(path), kind(value))
		}
		if err := boundNumber(schema, f, at(path)); err != nil {
			return err
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s should be a boolean, got %s", at(path), kind(value))
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s should be null, got %s", at(path), kind(value))
		}
	}
	return nil
}

// ValidateText parses text as JSON and validates it, which is the shape the answer actually arrives
// in: a model returns a string, and "did not parse" and "parsed but violated" are different failures
// a caller needs told apart.
func ValidateText(schema map[string]any, text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return errors.New("the run produced no output to validate")
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return fmt.Errorf("the output is not JSON (%s); a schema was demanded, so the answer had to be a JSON document", err)
	}
	return Validate(schema, decoded)
}

func boundNumber(schema map[string]any, f float64, where string) error {
	if raw, present := schema["minimum"]; present {
		if min, ok := asNumber(raw); ok && f < min {
			return fmt.Errorf("%s is %v, below the minimum %v", where, f, min)
		}
	}
	if raw, present := schema["maximum"]; present {
		if max, ok := asNumber(raw); ok && f > max {
			return fmt.Errorf("%s is %v, above the maximum %v", where, f, max)
		}
	}
	return nil
}

func boundInt(schema map[string]any, minKey, maxKey string, n int, where string) error {
	if raw, present := schema[minKey]; present {
		if min, ok := asNumber(raw); ok && float64(n) < min {
			return fmt.Errorf("%s is %d, below the minimum %v", where, n, min)
		}
	}
	if raw, present := schema[maxKey]; present {
		if max, ok := asNumber(raw); ok && float64(n) > max {
			return fmt.Errorf("%s is %d, above the maximum %v", where, n, max)
		}
	}
	return nil
}

func enumContains(raw any, value any) bool {
	list, ok := raw.([]any)
	if !ok {
		return true // Check refuses a non-array enum, so this is unreachable for accepted schemas
	}
	want, _ := json.Marshal(value)
	for _, candidate := range list {
		if got, _ := json.Marshal(candidate); string(got) == string(want) {
			return true
		}
	}
	return false
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// kind names a decoded JSON value's type in the caller's vocabulary, not Go's: a message saying
// "got float64" is about our decoder, not about their document.
func kind(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	case float64:
		if t == math.Trunc(t) {
			return "an integer"
		}
		return "a number"
	}
	return fmt.Sprintf("%T", v)
}

func render(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(b) > 120 {
		return string(b[:117]) + "..."
	}
	return string(b)
}

func at(path string) string {
	if path == "" {
		return "the output"
	}
	return "output." + strings.TrimPrefix(path, ".")
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func toStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toAnyMap(m map[string]bool) map[string]any {
	out := make(map[string]any, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}
