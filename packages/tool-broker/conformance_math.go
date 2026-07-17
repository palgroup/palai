package toolbroker

import (
	"encoding/json"
	"fmt"
)

// ConformanceMathAdd is the pure conformance tool palai.conformance.math.add. It
// sums two integers under strict input and output schemas, so it exercises schema
// validation, idempotent caching, and usage accounting without any side effect.
func ConformanceMathAdd() Tool {
	return Tool{
		Name: "palai.conformance.math.add",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"type": "integer"},
				"b": map[string]any{"type": "integer"},
			},
			"required":             []any{"a", "b"},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sum": map[string]any{"type": "integer"},
			},
			"required":             []any{"sum"},
			"additionalProperties": false,
		},
		Invoke: func(args map[string]any) (map[string]any, error) {
			a, err := toInt(args["a"])
			if err != nil {
				return nil, fmt.Errorf("argument a: %w", err)
			}
			b, err := toInt(args["b"])
			if err != nil {
				return nil, fmt.Errorf("argument b: %w", err)
			}
			return map[string]any{"sum": a + b}, nil
		},
	}
}

// validate checks value against a strict JSON Schema. It supports the subset the
// conformance tools use: object with properties, required, additionalProperties:
// false, and the scalar types integer, number, and string.
// ponytail: minimal in-repo schema subset; swap for a real JSON Schema validator
// if conformance tools ever need $ref, arrays, or enums.
func validate(schema map[string]any, value any) error {
	switch typ, _ := schema["type"].(string); typ {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object, got %T", value)
		}
		props, _ := schema["properties"].(map[string]any)
		for _, name := range toStrings(schema["required"]) {
			if _, present := obj[name]; !present {
				return fmt.Errorf("missing required property %q", name)
			}
		}
		if allow, ok := schema["additionalProperties"].(bool); ok && !allow {
			for key := range obj {
				if _, defined := props[key]; !defined {
					return fmt.Errorf("unexpected property %q", key)
				}
			}
		}
		for key, sub := range props {
			if raw, present := obj[key]; present {
				if subSchema, ok := sub.(map[string]any); ok {
					if err := validate(subSchema, raw); err != nil {
						return fmt.Errorf("property %q: %w", key, err)
					}
				}
			}
		}
		return nil
	case "integer":
		if !isInteger(value) {
			return fmt.Errorf("expected integer, got %v (%T)", value, value)
		}
		return nil
	case "number":
		if !isNumber(value) {
			return fmt.Errorf("expected number, got %T", value)
		}
		return nil
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("expected string, got %T", value)
		}
		return nil
	case "":
		return nil // an untyped schema constrains nothing
	default:
		return fmt.Errorf("unsupported schema type %q", typ)
	}
}

func toStrings(v any) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// isNumber and isInteger accept both native Go numerics and the float64/json.Number
// forms a JSON decode produces, so a tool call built in Go and one decoded from a
// provider stream validate identically.
func isNumber(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return true
	}
	return false
}

func isInteger(v any) bool {
	switch n := v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return n == float32(int64(n))
	case float64:
		return n == float64(int64(n))
	case json.Number:
		_, err := n.Int64()
		return err == nil
	}
	return false
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case int32:
		return int(n), nil
	case float64:
		return int(n), nil
	case float32:
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	}
	return 0, fmt.Errorf("not an integer: %T", v)
}
