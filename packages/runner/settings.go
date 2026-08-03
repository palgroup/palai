package runner

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// THE CONFIGURATION A RUNNING MACHINE IS GIVEN, and the answer it gives back.
//
// WHAT THIS FIXES. Before it, a machine received its configuration exactly once — in the answer to its own
// enrolment — so an operator who edited a pool's document in the panel changed nothing about any machine
// already running. Measured on the live stack 2026-08-03: the document asking for
// PALAI_RUNNER_CONCURRENCY=4 was written at 08:08 and the runner it configures had been up since 21:47 the
// previous evening. The panel said 4; the machine had never been told.
//
// AND THE MEASUREMENT NEARLY REPORTED THE OPPOSITE, which is why it is written down here rather than in a
// commit message. `docker inspect` on that runner shows PALAI_RUNNER_CONCURRENCY=4 — compose sets it, and
// planeIntDefault falls back to the environment when the plane sends nothing. The panel and the machine
// agreed on the number and no byte had passed between them. Any test that claims delivery must therefore
// use a value the machine's own environment does NOT hold, or it is measuring the harness.
//
// WHY THE MACHINE ASKS RATHER THAN BEING TOLD. cmd/runner's header states the property: "It opens no
// inbound port and writes no credential to disk." A push needs a listener on every machine — a rented Mac
// behind NAT, a laptop, a box in an office — which is a larger change to the trust boundary than the
// configuration it would deliver. The poll gives the operator the same outcome (an edit reaches the
// machine within one interval) with no inbound port and no new credential. The cost is latency, and it is
// bounded, configurable and stated rather than hidden.

// SettingsConfig is the input to one settings poll: where to ask, and the trust anchor and exact DNS
// identity the runner pins on every outbound connection — the same pair renewal and the lease session use,
// for the same reason.
type SettingsConfig struct {
	SettingsURL   string
	ControllerCAs *x509.CertPool
	ControllerDNS string
	Now           func() time.Time
}

// Verdicts a machine reports about one setting. They are the machine's OWN answer and the control plane
// does not derive them: whether a value can take effect without a restart is a fact about this binary's
// code, and a control plane that computed its own would be reporting the machine's state from a table
// instead of from the machine.
const (
	// VerdictApplied: this process changed its behaviour. The value is live now.
	VerdictApplied = "applied"
	// VerdictNotRead: this runner build reads no such setting, so nothing will happen to it — not now and
	// not after a restart. It is reported rather than silently dropped because the alternative is a panel
	// showing a saved value against a machine that will never act on it, which is the defect the whole
	// surface exists to prevent, moved one hop further away where it is harder to see.
	VerdictNotRead = "not_read"
	// NOTE ON THE THIRD VERDICT THAT IS NOT HERE. `pending_restart` — held, and live at the next restart —
	// has NO PRODUCER in this binary today, and declaring a constant nothing writes is the "declared, and
	// nothing happens" defect this tree keeps finding. Every setting cmd/runner takes from the plane
	// document is in settingApplier below, and every one of those is applied live; anything else is
	// genuinely not read. The day a setting is added that this process can only consume at exec, it gets an
	// entry in that table whose applier returns the new verdict, and the table is the only place to change.
)

// settingsDocument is the wire shape of the control plane's answer.
type settingsDocument struct {
	Revision int64             `json:"revision"`
	Settings map[string]string `json:"settings,omitempty"`
}

// settingsReport is the wire shape of what this machine says it did with the last document.
type settingsReport struct {
	Revision int64             `json:"revision"`
	Applied  map[string]string `json:"applied,omitempty"`
}

// Settings is one document as this machine resolved it.
type Settings struct {
	// Revision MOVES whenever either contributing document does — the pool's or this machine's. It is a
	// change detector rather than a citation: a machine compares it with the revision it is running to
	// decide whether anything needs applying, and must not read it as "the machine document is at N".
	Revision int64
	Settings map[string]string
}

// FetchSettings asks the control plane for this machine's current configuration, reporting in the same
// round trip what it did with the previous one.
//
// ONE ROUND TRIP FOR BOTH DIRECTIONS, deliberately. A separate report endpoint would let the two drift —
// a machine could be answering polls while its verdicts silently stopped arriving — and the report is only
// ever interesting alongside the document it is about.
//
// THE MACHINE IS IDENTIFIED BY ITS CERTIFICATE AND NEVER BY A FIELD IN THE BODY. There is no runner id on
// this wire, which is what makes it impossible for a machine to ask for another machine's configuration:
// the control plane reads the identity out of the TLS peer certificate's DNS name, exactly as renewal does.
func FetchSettings(ctx context.Context, current Identity, report Settings, config SettingsConfig) (Settings, error) {
	if current.Certificate.Leaf == nil || current.Certificate.PrivateKey == nil ||
		config.ControllerCAs == nil || config.ControllerDNS == "" || config.Now == nil {
		return Settings{}, errors.New("a settings poll requires the current identity, controller trust, DNS and clock")
	}
	if !strings.HasPrefix(config.SettingsURL, "https://") {
		return Settings{}, errors.New("settings URL must be outbound https")
	}

	body, err := json.Marshal(settingsReport{Revision: report.Revision, Applied: report.Settings})
	if err != nil {
		return Settings{}, fmt.Errorf("encode settings report: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.SettingsURL, bytes.NewReader(body))
	if err != nil {
		return Settings{}, fmt.Errorf("build settings request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	// renewTLS is reused verbatim rather than copied: this call presents the SAME current certificate and
	// pins the SAME controller CA and exact DNS identity that renewal does, and a second spelling of a TLS
	// configuration is a second thing that can be weakened on its own.
	transport := &http.Transport{TLSClientConfig: renewTLS(current, RenewConfig{
		ControllerCAs: config.ControllerCAs, ControllerDNS: config.ControllerDNS, Now: config.Now,
	}), Proxy: nil}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return Settings{}, fmt.Errorf("poll settings: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Settings{}, fmt.Errorf("settings poll rejected: %s", response.Status)
	}
	var decoded settingsDocument
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&decoded); err != nil {
		return Settings{}, fmt.Errorf("decode settings document: %w", err)
	}
	return Settings{Revision: decoded.Revision, Settings: decoded.Settings}, nil
}
