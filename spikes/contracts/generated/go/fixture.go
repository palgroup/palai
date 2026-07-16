// Code generated from the canonical Fixture schema; DO NOT EDIT.
package contractgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type OptionalNullableString struct {
	Present bool
	Null    bool
	Value   string
}

type Fixture struct {
	ID            string
	Note          OptionalNullableString
	Status        string
	Metadata      map[string]json.RawMessage
	Sequence      int64
	CreatedAt     string
	UnknownFields map[string]json.RawMessage
}

func DecodeFixture(data []byte) (Fixture, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return Fixture{}, fmt.Errorf("decode fixture: %w", err)
	}
	if fields == nil {
		return Fixture{}, errors.New("fixture must be an object")
	}
	fixture := Fixture{
		Metadata:      make(map[string]json.RawMessage),
		UnknownFields: make(map[string]json.RawMessage),
	}
	if err := decodeRequired(fields, "id", &fixture.ID); err != nil {
		return Fixture{}, err
	}
	if fixture.ID == "" {
		return Fixture{}, errors.New("fixture id must be non-empty")
	}
	if note, exists := fields["note"]; exists {
		fixture.Note.Present = true
		if bytes.Equal(bytes.TrimSpace(note), []byte("null")) {
			fixture.Note.Null = true
		} else if err := json.Unmarshal(note, &fixture.Note.Value); err != nil {
			return Fixture{}, fmt.Errorf("decode note: %w", err)
		}
	}
	if err := decodeRequired(fields, "status", &fixture.Status); err != nil {
		return Fixture{}, err
	}
	if fixture.Status == "" {
		return Fixture{}, errors.New("fixture status must be non-empty")
	}
	if err := decodeRequired(fields, "metadata", &fixture.Metadata); err != nil {
		return Fixture{}, err
	}
	if err := decodeRequired(fields, "sequence", &fixture.Sequence); err != nil {
		return Fixture{}, err
	}
	if fixture.Sequence < 0 {
		return Fixture{}, errors.New("fixture sequence must be non-negative")
	}
	if err := decodeRequired(fields, "created_at", &fixture.CreatedAt); err != nil {
		return Fixture{}, err
	}
	if _, err := time.Parse(time.RFC3339, fixture.CreatedAt); err != nil {
		return Fixture{}, fmt.Errorf("fixture created_at: %w", err)
	}
	for name, value := range fields {
		switch name {
		case "id", "note", "status", "metadata", "sequence", "created_at":
		default:
			fixture.UnknownFields[name] = append(json.RawMessage(nil), value...)
		}
	}
	return fixture, nil
}

func (fixture Fixture) Encode() ([]byte, error) {
	if fixture.ID == "" || fixture.Status == "" || fixture.Sequence < 0 {
		return nil, errors.New("fixture contains invalid required values")
	}
	if _, err := time.Parse(time.RFC3339, fixture.CreatedAt); err != nil {
		return nil, fmt.Errorf("fixture created_at: %w", err)
	}
	fields := make(map[string]json.RawMessage, len(fixture.UnknownFields)+6)
	for name, value := range fixture.UnknownFields {
		fields[name] = append(json.RawMessage(nil), value...)
	}
	for name, value := range map[string]any{
		"id":         fixture.ID,
		"status":     fixture.Status,
		"metadata":   fixture.Metadata,
		"sequence":   fixture.Sequence,
		"created_at": fixture.CreatedAt,
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", name, err)
		}
		fields[name] = encoded
	}
	if fixture.Note.Present {
		if fixture.Note.Null {
			fields["note"] = json.RawMessage("null")
		} else {
			encoded, err := json.Marshal(fixture.Note.Value)
			if err != nil {
				return nil, fmt.Errorf("encode note: %w", err)
			}
			fields["note"] = encoded
		}
	}
	return json.Marshal(fields)
}

func decodeRequired(fields map[string]json.RawMessage, name string, target any) error {
	value, exists := fields[name]
	if !exists {
		return fmt.Errorf("required field %s is missing", name)
	}
	if err := json.Unmarshal(value, target); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return nil
}
