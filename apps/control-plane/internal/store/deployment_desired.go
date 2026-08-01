package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

// GetDesiredConfig reads the current desired document, or nil when no operator has written one.
//
// NIL IS A REAL ANSWER AND IT MUST STAY DISTINGUISHABLE FROM AN EMPTY ONE. A machine nobody has written a
// desired configuration for is running on its compose file's defaults, and a screen that showed an empty
// document instead would imply the panel is in control when it is not. `{"settings":{}}` — the deliberate
// clear-everything write — is a document with no settings, which is a different fact and reports as one.
func (s *Store) GetDesiredConfig(ctx context.Context, scope middleware.Scope) (*api.DesiredDocument, error) {
	if !scope.HasScope("provision") {
		// Defence in depth. The handler gates this too; a store method that would answer an unscoped caller
		// is one refactor away from being called by something that forgot.
		return nil, errors.New("the desired configuration requires the provision capability")
	}
	var doc api.DesiredDocument
	var raw []byte
	err := s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), storage.Query("LatestDeploymentDesired")).
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
	settings, err := api.DecodeDesiredSettings(body)
	if err != nil {
		// The refusal SENTENCE is what reaches the caller, because every one of them names the setting and
		// says why. A bare 400 on this surface would send an operator to read Go source to find out which of
		// eleven fields they got wrong.
		return api.ProvisionResult{MissingField: err.Error()}, nil
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	var doc api.DesiredDocument
	doc.Settings = settings
	doc.WrittenBy = scope.Principal
	if err := s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), storage.Query("InsertDeploymentDesired"),
		encoded, scope.Principal).Scan(&doc.Revision, &doc.WrittenAt); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("write the desired configuration: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(map[string]any{
		"object":     "deployment_desired",
		"revision":   doc.Revision,
		"settings":   doc.Settings,
		"written_at": doc.WrittenAt,
		"written_by": doc.WrittenBy,
	})}, nil
}
