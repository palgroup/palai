package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/storage"
)

// The desired-configuration store (E29, migration 000052): the durable half of "no config should live
// locally — it must be pushed from the panel to the machine".
//
// SYSTEM-SCOPED, AND THAT IS THE ONLY UNUSUAL THING ABOUT THIS FILE. Every other method on this Store
// carries the verified tenant and lets RLS enforce the boundary the query claims. deployment_desired has
// no organization_id to carry: it is the configuration of the PROCESS every tenant on this machine shares,
// and four of its writable settings are the ADMISSION BOUNDS that exist to hold a tenant. So both
// statements run under storage.WithSystemScope, which is greppable on purpose — and the authority that
// replaces the tenant boundary is the `provision` capability the handler checks before either is reached.
//
// THE SCOPE IS STILL USED, for the one thing it legitimately says here: WHO wrote the revision. The
// principal comes from the VERIFIED credential and never from a body field.

// DesiredSettingsForPool answers one pool's desired settings, or nil when nobody has written any.
//
// IT TAKES NO middleware.Scope, which is the difference between it and GetDesiredConfig below, so the
// reason is written here rather than assumed. GetDesiredConfig serves an OPERATOR holding an API key and
// is gated on the `provision` capability. This one serves the ENROLMENT HANDLER, which has no API key and
// must never be given one: a runner authenticates with a certificate, and handing a fleet a
// provision-scoped credential so it can read its own configuration would be a far larger grant than the
// thing being read.
//
// WHAT REPLACES THE CAPABILITY IS THE POOL ID'S PROVENANCE. The caller passes the pool from the RESOLVED
// GRANT — the credential chain's verdict about which pool this machine's key belongs to — never a pool the
// request named. A machine therefore cannot read another pool's document by declaring it, which is the
// property the capability check stands in for here. The one caller is RunnerGateway.handleEnroll; a second
// caller sourcing its poolID from a request body would defeat this and must not be added.
//
// A pool nobody has configured answers nil rather than an error: that machine runs on the configuration it
// was started with, which is what every runner did before this document existed.
func (s *Store) DesiredSettingsForPool(ctx context.Context, poolID string) (map[string]string, error) {
	if poolID == "" {
		return nil, nil
	}
	var (
		revision  int64
		raw       []byte
		writtenAt time.Time
		writtenBy string
	)
	err := s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), storage.Query("LatestDeploymentDesired"),
		api.PlaneRunnerPool, poolID).Scan(&revision, &raw, &writtenAt, &writtenBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pool %s desired configuration: %w", poolID, err)
	}
	var settings map[string]string
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("decode pool %s desired document revision %d: %w", poolID, revision, err)
	}
	return settings, nil
}

// GetDesiredConfig reads the current desired document, or nil when no operator has written one.
//
// NIL IS A REAL ANSWER AND IT MUST STAY DISTINGUISHABLE FROM AN EMPTY ONE. A machine nobody has written a
// desired configuration for is running on its compose file's defaults, and a screen that showed an empty
// document instead would imply the panel is in control when it is not. `{"settings":{}}` — the deliberate
// clear-everything write — is a document with no settings, which is a different fact and reports as one.
func (s *Store) GetDesiredConfig(ctx context.Context, scope middleware.Scope, plane, scopeID string) (*api.DesiredDocument, error) {
	if !scope.HasScope("provision") {
		// Defence in depth. The handler gates this too; a store method that would answer an unscoped caller
		// is one refactor away from being called by something that forgot.
		return nil, errors.New("the desired configuration requires the provision capability")
	}
	doc := api.DesiredDocument{Plane: plane, Scope: scopeID}
	var raw []byte
	err := s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), storage.Query("LatestDeploymentDesired"), plane, scopeID).
		Scan(&doc.Revision, &raw, &doc.WrittenAt, &doc.WrittenBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the desired configuration: %w", err)
	}
	if err := json.Unmarshal(raw, &doc.Settings); err != nil {
		return nil, fmt.Errorf("decode desired document revision %d: %w", doc.Revision, err)
	}
	if doc.Settings == nil {
		doc.Settings = map[string]string{}
	}
	return &doc, nil
}

// PutDesiredConfig appends a revision holding the WHOLE document.
//
// IT DECODES THROUGH api.DecodeDesiredSettings, which is the shipped allow-list and value grammar — not a
// copy of them. A second copy of the rules here is the defect the deployment surface was built to expose,
// one layer up: two validators that agree today and diverge on the day one is edited.
//
// REPLACE, NOT MERGE. A key absent from the body means "stop deciding this setting", and the next bring-up
// then leaves the deployment's own default in place. A merge would make removal unexpressible and would
// mean the document could only ever grow — which is how a stack ends up carrying a setting nobody
// remembers asking for.
func (s *Store) PutDesiredConfig(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	if !scope.HasScope("provision") {
		return api.ProvisionResult{}, errors.New("the desired configuration requires the provision capability")
	}
	write, err := api.DecodeDesiredSettings(body)
	if err != nil {
		// The refusal SENTENCE is what reaches the caller, because every one of them names the setting and
		// says why. A bare 400 on this surface would send an operator to read Go source to find out which of
		// eleven fields they got wrong.
		return api.ProvisionResult{MissingField: err.Error()}, nil
	}
	encoded, err := json.Marshal(write.Settings)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	doc := api.DesiredDocument{Plane: write.Plane, Scope: write.Scope, Settings: write.Settings, WrittenBy: scope.Principal}
	if err := s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), storage.Query("InsertDeploymentDesired"),
		write.Plane, write.Scope, encoded, scope.Principal).Scan(&doc.Revision, &doc.WrittenAt); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("write the desired configuration: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(map[string]any{
		"object":     "deployment_desired",
		"plane":      doc.Plane,
		"scope_id":   doc.Scope,
		"revision":   doc.Revision,
		"settings":   doc.Settings,
		"written_at": doc.WrittenAt,
		"written_by": doc.WrittenBy,
	})}, nil
}
