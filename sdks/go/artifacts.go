package palai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Artifacts is the /v1/artifacts resource group (apps/control-plane/api/artifacts.go).
//
// This SDK ships the CREATE and nothing else, which is the opposite of what the surface's history would
// suggest: the read routes (metadata, byte download, a response's artifact list) have existed since E13 T5
// and the write verb did not exist at all. The create is here because it is the one an out-of-process
// client cannot work around — a run's input may NAME an artifact through an `image_ref` content item, and
// without this call there is no way for a client of the public API to ever produce an id to put there. The
// reads are a later task's surface if a caller needs them.
type Artifacts struct{ client *Client }

// Artifact is a created artifact's projection.
type Artifact struct {
	ID        string                     `json:"id"`
	Object    string                     `json:"object"`
	MediaType string                     `json:"media_type"`
	SizeBytes int64                      `json:"size_bytes"`
	Extra     map[string]json.RawMessage `json:"-"`
}

func (a *Artifact) UnmarshalJSON(raw []byte) error {
	type alias Artifact
	var v alias
	if err := forwardUnmarshal(raw, &v, &v.Extra); err != nil {
		return err
	}
	*a = Artifact(v)
	return nil
}

func (a Artifact) MarshalJSON() ([]byte, error) {
	type alias Artifact
	return forwardMarshal(alias(a), a.Extra)
}

// Create uploads image bytes and returns the artifact whose ID a run's `image_ref` content item names
// (POST /v1/artifacts).
//
// THE BYTES ARE THE WHOLE REQUEST. There is no filename, media type or classification parameter, and that
// is the server's contract rather than a thin binding: it SNIFFS the bytes and refuses anything that is not
// a PNG, JPEG, GIF or WEBP image, so a declared type would be a value the server ignores — and a parameter
// the server ignores is a lie in a function signature. A caller who knows what it is holding still learns
// what the server decided, from the returned MediaType.
//
// Typical refusals, all typed *APIError: 413 for bytes over the server's ceiling, 415 for anything that
// does not sniff as one of those four image types, 401 for a missing or rejected key.
//
// NO Idempotency-Key RIDES THIS CALL, matching the server (see the route's own comment). The id is
// server-minted, so a retry would create a SECOND artifact rather than replaying the first — which is why
// this create also leaves requestOptions.idempotent false: a connection torn after the server committed
// fails closed here instead of silently storing the same screenshot twice.
func (a *Artifacts) Create(ctx context.Context, content []byte, opts ...CallOption) (*Artifact, error) {
	if len(content) == 0 {
		// Refused before the round trip: the server answers 400 for an empty body, and paying a request to
		// be told so is a cost with no information in it.
		return nil, errors.New("palai: an artifact create needs bytes")
	}
	o := requestOptions{
		rawBody: content,
		// The server sniffs and does not read this header, but a request that declares nothing about its
		// payload is harder to read in a proxy log than one that says "opaque bytes". It deliberately does
		// NOT claim an image type this SDK has not verified.
		rawContentType: "application/octet-stream",
	}
	applyCallOptions(&o, opts)
	var out Artifact
	if err := a.client.doJSON(ctx, http.MethodPost, "/v1/artifacts", o, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
