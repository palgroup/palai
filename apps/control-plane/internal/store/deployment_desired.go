package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/runner"
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
	settings, _, err := s.desiredScope(ctx, api.PlaneRunnerPool, poolID)
	return settings, err
}

// desiredScope reads ONE (plane, scope) document and the revision it came from, or (nil, 0) when nobody
// has written one. It is the single place the three callers above and below share, so the system scope,
// the not-found convention and the decode error all have one spelling.
func (s *Store) desiredScope(ctx context.Context, plane, scopeID string) (map[string]string, int64, error) {
	var (
		revision  int64
		raw       []byte
		writtenAt time.Time
		writtenBy string
	)
	err := s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), storage.Query("LatestDeploymentDesired"),
		plane, scopeID).Scan(&revision, &raw, &writtenAt, &writtenBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read %s %s desired configuration: %w", plane, scopeID, err)
	}
	var settings map[string]string
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, 0, fmt.Errorf("decode %s %s desired document revision %d: %w", plane, scopeID, revision, err)
	}
	return settings, revision, nil
}

// DesiredSettingsForMachine answers ONE machine its effective configuration: its pool's document with its
// own machine document laid over the top, key by key, plus the revision that produced the result.
//
// THE OVERLAY IS COMPUTED HERE AND NEVER STORED FLATTENED. A machine document holds only the keys that
// machine decides differently; everything else keeps coming from the pool, so a later pool edit still
// reaches a machine that has been individually configured. Storing the merged result at write time was the
// alternative and it is the "declared, and nothing happens" defect one layer down: the pool edit would
// land, the screen would show it saved, and the machine it was written for would be the one machine it
// could never reach.
//
// THE REVISION RETURNED IS THE HIGHER OF THE TWO THAT CONTRIBUTED, and it is a CHANGE DETECTOR rather than
// a citation. A machine compares it with the one it is running to decide whether anything needs applying,
// so what it must do is MOVE whenever either document does — which max() gives, because revision is a
// single generated identity shared by both planes and therefore totally ordered across them. It is
// deliberately not a citation: revision 7 here may be the pool's while the machine's document is 5, and
// nothing should read it as "the machine document is at 7".
//
// A machine whose pool and machine documents are both absent gets (nil, 0): it runs on the configuration it
// was started with, which is what every runner did before any of this existed, and 0 is a revision the
// journal can never mint (it is a generated identity starting at 1) so it is safe as "nothing".
func (s *Store) DesiredSettingsForMachine(ctx context.Context, poolID, runnerID string) (map[string]string, int64, error) {
	pool, poolRevision, err := s.desiredScope(ctx, api.PlaneRunnerPool, poolID)
	if err != nil {
		return nil, 0, err
	}
	machine, machineRevision, err := s.desiredScope(ctx, api.PlaneRunnerMachine, runnerID)
	if err != nil {
		return nil, 0, err
	}
	if pool == nil && machine == nil {
		return nil, 0, nil
	}
	effective := make(map[string]string, len(pool)+len(machine))
	for name, value := range pool {
		effective[name] = value
	}
	// MACHINE WINS, which is the only precedence that makes the scope worth having: an operator reaches for
	// a machine document precisely to say "this Mac is different from the rest of its pool".
	for name, value := range machine {
		effective[name] = value
	}
	revision := poolRevision
	if machineRevision > revision {
		revision = machineRevision
	}
	return effective, revision, nil
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
	// A MACHINE DOCUMENT MUST NAME A MACHINE THAT EXISTS. The decoder cannot check this — it holds no store
	// handle, deliberately — so the check lives here, at the write path, which is also the only layer that
	// can say which id it did not find.
	//
	// IT IS THE ONE SCOPE THAT IS CHECKED, and the asymmetry is deliberate rather than unfinished. A pool id
	// that matches nothing is a document that configures an empty set and starts working the moment a
	// machine enrols into that pool — a plausible order of operations for an operator setting a pool up. A
	// runner id that matches nothing is different in kind: runner ids are MINTED BY THE SERVER at enrolment
	// (RunnerGateway.mintRunnerID), so an operator cannot know one before the machine exists. An unmatched
	// runner id is therefore a typo or a stale copy, every time, and RecordRunPool's lesson says a write
	// that matches no row must refuse rather than land silently.
	if write.Plane == api.PlaneRunnerMachine {
		var exists bool
		if err := s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), storage.Query("RunnerExistsForConfig"), write.Scope).Scan(&exists); err != nil {
			return api.ProvisionResult{}, fmt.Errorf("check machine %s exists: %w", write.Scope, err)
		}
		if !exists {
			return api.ProvisionResult{MissingField: fmt.Sprintf(
				"desired configuration refused: no machine carries the id %q. A runner id is minted by this control plane when the "+
					"machine enrols, so it cannot be chosen in advance — take one from GET /v1/runners. To configure a machine that "+
					"has not enrolled yet, write the document against the POOL it will join instead; a machine inherits its pool's "+
					"configuration the moment it arrives", write.Scope)}, nil
		}
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

// RecordRunnerConfigReport stores what a machine says it did with the configuration it was sent, and
// reports whether a registry row MATCHED.
//
// THE BOOLEAN IS THE POINT AND IT IS NOT AN ERROR. A certificate that matches no row is a REAL and expected
// state: a runner that enrolled before the registry existed rolls its certificate forward over its own mTLS
// identity and never re-enrols, so it stays unknown for as long as it lives. Returning that as an error
// would make the caller choose between treating a normal machine as a fault and swallowing genuine store
// failures with it — RecordRunPool's lesson pointed the other way. false, nil says "nothing was written and
// nothing is wrong"; a non-nil error says the write failed.
//
// THE VERDICTS ARE THE MACHINE'S AND THIS METHOD DOES NOT SECOND-GUESS THEM. `applied` means that process
// changed its behaviour; `pending_restart` means it holds the value and is still running the old one. The
// control plane cannot derive either — whether a setting can be applied without a restart is a fact about
// the RUNNER's code, and a control plane that computed its own answer would be reporting a machine's state
// from a table instead of from the machine. That is the distinction the whole surface exists to preserve:
// a panel showing "saved" for a value no process has taken is the defect, and it does not get better by
// being computed more confidently.
//
// IT IS SYSTEM-SCOPED because it is keyed on a certificate rather than on a tenant: the machine proved its
// identity in TLS, and the row it may write is the one its own certificate names. There is no tenant in
// that sentence to scope by.
// settingConcurrency is the one setting whose applied value is also a SERVER-side ceiling, so the two
// halves must be the same word in both places.
const settingConcurrency = "PALAI_RUNNER_CONCURRENCY"

func (s *Store) RecordRunnerConfigReport(ctx context.Context, dns string, revision int64, applied map[string]string, at time.Time) (bool, error) {
	if dns == "" {
		return false, nil
	}
	if applied == nil {
		// A NULL column and an empty object are two different facts and both are reachable: never reported,
		// versus reported a document with nothing in it. `{}` is what a machine sends when the plane has no
		// document for it, and it must not read back as "never reported".
		applied = map[string]string{}
	}
	encoded, err := json.Marshal(applied)
	if err != nil {
		return false, err
	}
	var id, poolID string
	err = s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), storage.Query("RecordRunnerConfigReport"),
		dns, revision, encoded, at).Scan(&id, &poolID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record runner %s configuration report: %w", dns, err)
	}
	if err := s.setCapacityFromReport(ctx, id, poolID, revision, applied); err != nil {
		return true, err
	}
	return true, nil
}

// setCapacityFromReport turns the concurrency the machine SAYS IT APPLIED into the server's occupancy
// ceiling, which is the half of §3.6 that had no writer.
//
// ‼️ `capacity` WAS WRITTEN ONCE, AT ENROLMENT, FROM A VARIABLE THE DEVICE PATH DELETED. RecoverRunner's
// comment already calls it "the ADMIN plane's number since plan §3.6" and deliberately refuses to let a
// re-enrolment overwrite it — correctly, and leaving the admin plane with no way to set it at all. A
// packaged agent reads no PALAI_ variables, so it enrolled with `capacity = 0`, which AcquireLease reads
// as NO CEILING, while the desired document had it serving N lease loops. Placement then handed it more
// occupancies than it had loops, and the extra sessions waited on a loop that never frees.
//
// TWO CONDITIONS, AND BOTH ARE ABOUT NOT WRITING A NUMBER NOBODY IS SERVING:
//
//   - the verdict must be APPLIED. `not_read` means this build has no such setting and `refused:` means
//     it would not take the value, and ceiling a machine at a number it told us it is not running is
//     worse than leaving the old one.
//   - the report's revision must be the one this machine is currently owed. A machine reporting an older
//     revision applied an older value; resolving today's document for it would write a ceiling from a
//     document it has never seen, which is the stale-measurement trap this tree keeps finding.
//
// A machine that has fallen behind keeps its previous ceiling until its next report catches up, which is
// the honest answer rather than a confident wrong one.
func (s *Store) setCapacityFromReport(ctx context.Context, runnerID, poolID string, revision int64, applied map[string]string) error {
	if applied[settingConcurrency] != runner.VerdictApplied {
		return nil
	}
	settings, current, err := s.DesiredSettingsForMachine(ctx, poolID, runnerID)
	if err != nil {
		return fmt.Errorf("resolve desired settings for runner %s: %w", runnerID, err)
	}
	if current != revision {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(settings[settingConcurrency]))
	if err != nil || n < 1 {
		// The machine reported APPLIED for a value this plane cannot parse, which means the two disagree
		// about the grammar. Leaving the ceiling alone keeps the disagreement visible in config_applied
		// rather than resolving it into a number.
		return nil
	}
	var id string
	err = s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), storage.Query("SetRunnerCapacity"), runnerID, n).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("set runner %s capacity to %d: %w", runnerID, n, err)
	}
	return nil
}
