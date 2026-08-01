package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// THE DESIRED CONFIGURATION — a document the panel writes and the bring-up applies.
//
// WHY IT IS NOT AN EDIT CONTROL ON THE EFFECTIVE VALUE. Thirty-two of thirty-five catalogued settings are
// `bring_up`: read once from the process environment, which is fixed at exec. Measured on the live stack
// 2026-08-01:
//
//	curl -s .../v1/deployment | jq -r '.settings[].mutability' | sort | uniq -c
//	  32 bring_up
//	   3 bring_up_default_only
//
// deployment.go's own header says what a form on top of that would be: "a surface offering an 'edit'
// control for any of them would be the exact defect this repository keeps finding — declared, and nothing
// happens." So the panel writes a DESIRED value, durably and server-side; `palai up` reads the document and
// exports it into the environment it brings the process up with; and GET /v1/deployment reports EFFECTIVE
// against DESIRED so the gap between them is a row on a screen rather than a `docker inspect`.
//
// WHY IT IS NOT config_policy. That is a JSONB column on the PROJECTS table
// (storage/migrations/000005_config_revisions.up.sql:16) written per project through PATCH /v1/projects/{id}.
// A dispatch worker count is a property of the PROCESS every project on this machine shares. Putting a
// machine-wide switch behind a project picker would make it look per-project to the one reader who most
// needs to know it is not — and there is a second, harder reason: a project's document is tenant-scoped and
// this one cannot be. A tenant that could raise its own PALAI_MAX_CONCURRENT_RUNS would be setting the
// admission bound that exists to hold it. The desired document is SYSTEM-scoped, and migration 000052
// carries no organization_id so that it cannot become anything else.

// ErrDesiredRefused is the typed refusal a store returns when a document is rejected on its way in. The
// message is operator-facing and names the setting.
var ErrDesiredRefused = errors.New("desired configuration refused")

// DesiredDocument is one revision of the desired configuration: the settings an operator has decided about,
// with who decided and when.
//
// SETTINGS IS PRESENCE-KEYED AND THERE IS NO EMPTY-STRING VALUE, which is the one modelling decision in
// this file worth arguing about. A key that is present means "the operator decided this"; a key that is
// absent means "whatever the deployment's own default is". An empty string would be a third thing that
// looks like the second and behaves like the first, and this tree has already paid for that: migration
// 000048's header writes down the rule ("” is ONE kind of nothing where NULL plus ” would be two"). So
// going back to the compose default is expressed by REMOVING the key, and the write path refuses "".
type DesiredDocument struct {
	Revision int64 `json:"revision"`
	// Plane and Scope name WHICH PROCESS this document configures. They are carried on the document rather
	// than left implicit in the caller, so a screen and a bring-up both read the scope off the same object
	// they read the settings off — the pair that has to agree travels together.
	Plane     string            `json:"plane"`
	Scope     string            `json:"scope_id"`
	Settings  map[string]string `json:"settings"`
	WrittenAt time.Time         `json:"written_at"`
	WrittenBy string            `json:"written_by"`
}

// DesiredWrite is one decoded write: which scope, and what for it.
type DesiredWrite struct {
	Plane    string
	Scope    string
	Settings map[string]string
}

// maxDesiredSettings bounds a document at slightly more than the catalogue can hold, so a hostile body
// cannot make the server allocate before the per-key allow-list has had a chance to refuse anything.
const maxDesiredSettings = 64

// maxDesiredValueBytes bounds one value. Every accepted grammar is far shorter; this is the size at which
// the decoder stops reading rather than the size a real value has.
const maxDesiredValueBytes = 256

// DecodeDesiredSettings turns a request body into the settings map a store may persist, or a refusal.
//
// IT ASSUMES THE BODY IS HOSTILE. Four gates, in this order, and the order matters — the allow-list runs
// before any value is looked at, so a value grammar is never asked about a setting that has no business
// being here:
//
//  1. STRICT DECODE. An unknown top-level field is a 400 rather than a silently-ignored field. This tree
//     has been bitten from BOTH sides of that: `config_policy.pool` was 400'd by DisallowUnknownFields
//     while a plan read the route table and called the write path shipped.
//  2. THE ALLOW-LIST. desiredWritable() is computed from the catalogue and drops every path-kind setting
//     structurally. A name that is not in it is refused BY NAME, so a caller learns which key was wrong
//     rather than that "something" was.
//  3. THE VALUE GRAMMAR. Each writable setting declares the standard-library call its own reader makes;
//     a value that reader would coerce is refused here rather than stored and misreported later.
//  4. THE SHAPE OF THE STRING ITSELF. No control character, no newline, bounded length — enforced by every
//     grammar, because the value's journey ends as a `${VAR}` interpolation into a YAML scalar and a
//     newline there is a structure edit rather than a value.
func DecodeDesiredSettings(body []byte) (DesiredWrite, error) {
	var in struct {
		// Plane/Scope are OPTIONAL and default to the control plane, so every existing caller's body is
		// unchanged. They are ACCEPTED-AND-REFUSED rather than rejected as unknown fields when they name the
		// runner plane, and that is the difference between a specific answer and a shrug: a client written
		// for the plane this deployment does not yet serve gets a sentence naming the missing reader.
		Plane    string            `json:"plane"`
		Scope    string            `json:"scope_id"`
		Settings map[string]string `json:"settings"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return DesiredWrite{}, fmt.Errorf("%w: the body must be {\"settings\": {\"PALAI_X\": \"value\"}} with an optional \"plane\", and carry no other field: %v", ErrDesiredRefused, err)
	}
	if dec.More() {
		return DesiredWrite{}, fmt.Errorf("%w: the body carries more than one JSON value", ErrDesiredRefused)
	}
	if in.Plane == "" {
		in.Plane = planeControlPlane
	}
	// THE SCOPE GATE, AND IT RUNS FIRST. A document is only worth validating once it is going somewhere a
	// reader will look, and the refusal for the runner plane names the reader that does not exist rather
	// than pretending the plane is unknown — the model has it, this deployment has no second binary reading
	// it, and saying so is what stops a row landing that nothing consults.
	switch in.Plane {
	case planeControlPlane:
		if in.Scope != "" {
			return DesiredWrite{}, fmt.Errorf("%w: the control plane is a SINGLETON — one process per deployment — so it takes no scope_id (got %q)", ErrDesiredRefused, in.Scope)
		}
	case planeRunnerPool:
		return DesiredWrite{}, fmt.Errorf("%w: this deployment cannot apply a %s document yet. The storage is scoped for it and the "+
			"catalogue can carry it, but the READER is a second binary: cmd/runner reads its environment at exec (main.go:117 for "+
			"PALAI_RUNNER_CONCURRENCY, main.go:120 for PALAI_WORKSPACE_ROOT) and nothing hands it a document. A row written here would "+
			"be a setting no machine ever sees. Configure a pool's posture and enrolment through POST/PATCH /v1/runner-pools, which "+
			"the registry does enforce", ErrDesiredRefused, planeRunnerPool)
	default:
		return DesiredWrite{}, fmt.Errorf("%w: %q is not a plane this deployment knows. The two are %q and %q", ErrDesiredRefused, in.Plane, planeControlPlane, planeRunnerPool)
	}
	if in.Settings == nil {
		// An ABSENT `settings` is refused rather than read as "clear everything". Clearing every setting is a
		// real operation and it is spelled `{"settings":{}}`; a body that forgot the field would otherwise
		// silently perform the most destructive write this surface has.
		return DesiredWrite{}, fmt.Errorf("%w: `settings` is required — send {\"settings\":{}} to clear every desired value", ErrDesiredRefused)
	}
	if len(in.Settings) > maxDesiredSettings {
		return DesiredWrite{}, fmt.Errorf("%w: %d settings; a document may carry at most %d", ErrDesiredRefused, len(in.Settings), maxDesiredSettings)
	}

	writable := desiredWritable()
	out := make(map[string]string, len(in.Settings))
	for name, value := range in.Settings {
		entry, ok := writable[name]
		if !ok {
			if reason := nonDesiredReason[name]; reason != "" {
				return DesiredWrite{}, fmt.Errorf("%w: %s is not writable from this surface — %s", ErrDesiredRefused, name, reason)
			}
			return DesiredWrite{}, fmt.Errorf("%w: %s is not a setting this deployment declares. The catalogue is an allow-list: "+
				"a variable nobody has decided about is not writable, whatever it is called", ErrDesiredRefused, name)
		}
		// THE PLANE MUST MATCH. A control-plane document may only carry settings the control plane reads —
		// otherwise the value is exported into a process that never looks at it, and the screen reports a
		// change that reached nothing. The catalogue is entirely control-plane today, so this cannot fire
		// yet; it is written now because the day it can fire is the day somebody adds a runner-plane entry,
		// and that is exactly when nobody will be looking here.
		if planeOf(entry) != in.Plane {
			return DesiredWrite{}, fmt.Errorf("%w: %s is read on the %s plane and this document configures %s. A value written "+
				"into the wrong plane's document is exported into a process that never reads it", ErrDesiredRefused, name, planeOf(entry), in.Plane)
		}
		if err := validateDesiredValue(entry, value); err != nil {
			return DesiredWrite{}, fmt.Errorf("%w: %s: %v", ErrDesiredRefused, name, err)
		}
		out[name] = value
	}
	return DesiredWrite{Plane: in.Plane, Scope: in.Scope, Settings: out}, nil
}

// validateDesiredValue enforces one setting's grammar. Every branch refuses rather than repairs: a trimmed
// value is a value the operator did not type, and the whole point of the grammar is that what is stored is
// what the reader will parse.
func validateDesiredValue(entry catalogueEntry, value string) error {
	if value == "" {
		return errors.New("an empty value is not a setting. Remove the key to go back to this deployment's own default — " +
			"an empty string would be stored, exported, and read by the process as unset, which is a third state that looks like the second")
	}
	if len(value) > maxDesiredValueBytes {
		return fmt.Errorf("%d bytes; at most %d", len(value), maxDesiredValueBytes)
	}
	// THE SHAPE GATE, ahead of every grammar. It is stated separately rather than left to the grammars
	// because a future grammar that forgot it would be the whole exposure: the value ends up interpolated
	// into a YAML scalar in compose.yaml, where a newline is a structure edit.
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("carries a control character (%#U). This value is exported into a process environment and "+
				"interpolated into a compose YAML scalar; a newline there is a structure edit rather than a value", r)
		}
	}
	if strings.ContainsAny(value, "$`\\\"'") {
		return errors.New("carries one of $ ` \\ \" ' — none of them appear in any value this deployment's readers parse, " +
			"and each one means something on the way to the process (a second round of compose interpolation, a shell quote)")
	}

	switch entry.DesiredValue {
	case desiredInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("not an integer this binary's own reader would parse (strconv.Atoi: %v). "+
				"It reads an unparseable value as its default and says nothing, so the panel would show a number the process is not running", err)
		}
		if n < 0 {
			return fmt.Errorf("%d is negative", n)
		}
		if strconv.Itoa(n) != value {
			// `+1`, `007`, ` 1` — Atoi accepts some of these and the round trip is what catches the rest. The
			// value STORED must be the value the reader parses back to, or the screen and the process disagree.
			return fmt.Errorf("%q does not survive a round trip through the reader's own parse (it reads back as %q). "+
				"Send the canonical form", value, strconv.Itoa(n))
		}
	case desiredRate:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("not a number this binary's own reader would parse (strconv.ParseFloat: %v)", err)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("is not a finite number")
		}
		if f < 0 {
			return fmt.Errorf("%v is negative", f)
		}
	case desiredDuration:
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("not a Go duration (time.ParseDuration: %v). `10min` is ten minutes to a reader and "+
				"UNPARSEABLE to this binary, which then uses its default and says nothing — write `10m`", err)
		}
		if d < 0 {
			return fmt.Errorf("%s is negative", d)
		}
	case desiredToken:
		if !desiredTokenPattern.MatchString(value) {
			return fmt.Errorf("does not match %s — a provider selector or a model id, and nothing that could act "+
				"on the way to the process", desiredTokenPattern.String())
		}
	default:
		// Unreachable through desiredWritable(), which admits only entries with a grammar. It is a refusal
		// rather than a fallthrough because the failure mode of a new grammar nobody implemented must be
		// "nothing is accepted", never "everything is".
		return fmt.Errorf("declares the value grammar %q, which this binary does not implement", entry.DesiredValue)
	}
	return nil
}

// desiredView is the desired document as GET /v1/deployment reports it, plus the two facts a screen needs
// that the document itself does not carry: whether a bring-up is pending, and which settings differ.
type desiredView struct {
	// Plane and Scope say WHICH PROCESS this document configures, on the wire, so a screen cannot render a
	// control-plane document under a machine-shaped heading — the misreading this whole scoping pass exists
	// to prevent.
	Plane     string    `json:"plane"`
	Scope     string    `json:"scope_id"`
	Revision  int64     `json:"revision"`
	WrittenAt time.Time `json:"written_at"`
	WrittenBy string    `json:"written_by"`
	// Pending is len(Drifted) > 0, served rather than left to each client to derive, so two clients cannot
	// disagree about whether this machine is up to date.
	Pending bool `json:"pending"`
	// Drifted names the settings whose desired value is not what this process is running with, sorted so a
	// screen and a CLI print the same list.
	Drifted []string `json:"drifted"`
}

// desiredDrift compares the desired document against what THIS PROCESS holds, and it compares the RAW
// ENVIRONMENT STRINGS rather than the behaviour they produce.
//
// THAT IS THE CORRECT COMPARISON AND THE OTHER ONE IS A TRAP. `PALAI_DISPATCH_WORKERS` unset means ONE
// worker to DispatchWorkers() and ZERO to compose.yaml, which defaults it to 0 — so "unset" and "1" are the
// same behaviour to the code and opposite behaviours to the deployment. A drift check that compared
// effective behaviour would call desired=1 against an unset process "no drift" and leave the operator on
// the queued-only stack the whole surface was built to expose. What the next bring-up will EXPORT is the
// string; the string is what is compared.
func desiredDrift(doc *DesiredDocument, effective func(string) string) []string {
	if doc == nil {
		return nil
	}
	var drifted []string
	for name, want := range doc.Settings {
		if effective(name) != want {
			drifted = append(drifted, name)
		}
	}
	sort.Strings(drifted)
	return drifted
}

// DesiredConfigAPI is the store seam for the desired configuration. internal/store implements it.
//
// IT IS AN OPTION AND NOT A POSITIONAL PARAMETER, and the nil case is a REAL posture rather than a test
// convenience: a control plane wired without a durable spine (the Docker-free conformance HTTP tier) has
// nowhere to keep a desired document, and GET /v1/deployment on it must report the effective configuration
// with NO desired block — not an empty one. An empty block would say "an operator wrote a document with
// nothing in it", which is a different fact and the one that would make a screen claim the panel is in
// control of a machine it cannot write to.
type DesiredConfigAPI interface {
	// GetDesiredConfig returns the current document for ONE plane/scope, or nil when no revision has ever
	// been written for it. Nil is distinct from a document with no settings, which is the deliberate
	// clear-everything write.
	GetDesiredConfig(ctx context.Context, scope middleware.Scope, plane, scopeID string) (*DesiredDocument, error)
	PutDesiredConfig(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
}

// desiredHandler serves PUT /v1/deployment/desired.
//
// PUT AND NOT PATCH, because the write REPLACES the document — which is what makes "stop deciding this
// setting" expressible as an absent key. A PATCH would advertise a merge, and under a merge the document
// could only ever grow.
type desiredHandler struct {
	desired DesiredConfigAPI
}

func (h *desiredHandler) put(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	// The SAME capability the read is gated on, and for a stronger version of the same reason: this writes
	// the machine's admission bounds, its dispatch posture and its model default. A project-scoped key handed
	// to a run has no business deciding how many runs the machine will admit.
	if !scope.HasScope(provisionScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "the request body is too large")
		return
	}
	out, err := h.desired.PutDesiredConfig(r.Context(), scope, raw)
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "the desired configuration could not be written")
	case out.MissingField != "":
		// The store hands back the refusal SENTENCE, which names the setting and says why. This surface has
		// eleven writable fields and twenty-four refused ones; "invalid_request" on its own would send an
		// operator to read Go source.
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", out.MissingField)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out.Body)
	}
}
