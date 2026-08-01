package modelbroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	// ErrUnknownProvider is returned when no adapter is registered for a provider.
	ErrUnknownProvider = errors.New("unknown_provider")
	// ErrUnknownSecret is returned when a SecretRef resolves to no credential.
	ErrUnknownSecret = errors.New("unknown_secret")
)

// SecretResolver redeems a SecretRef name to its credential value. Only the broker
// executor calls it, and only at call time, so the value never lives on a request.
type SecretResolver interface {
	Redeem(ref SecretRef) (string, error)
}

// StaticResolver maps SecretRefs to literal values. It backs the deterministic
// conformance and security suites, never a live call.
type StaticResolver map[SecretRef]string

// Redeem returns the mapped value or ErrUnknownSecret.
func (r StaticResolver) Redeem(ref SecretRef) (string, error) {
	v, ok := r[ref]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownSecret, ref)
	}
	return v, nil
}

// EnvResolver maps a SecretRef to an environment variable name and reads the value
// at redemption time, so a live credential lives only in process memory and never
// in a command argument, request, or log.
type EnvResolver map[SecretRef]string

// Redeem reads the mapped environment variable, treating an empty value as absent.
func (r EnvResolver) Redeem(ref SecretRef) (string, error) {
	name, ok := r[ref]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownSecret, ref)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%w: environment variable %s is unset", ErrUnknownSecret, name)
	}
	return v, nil
}

// ChainResolver tries each resolver in order and returns the first hit. It exists because a deployment's
// credential sources are plural and always were: the env bridge for the deployment default, a static entry
// for the deterministic fake. Before E29 that plurality was expressed by building a DIFFERENT broker per
// case, which is exactly how the adapter map came to be gated on an env var — the two concerns were welded
// together in one constructor. Only an ErrUnknownSecret falls through; a real failure (a store outage, a
// decrypt failure) stops the chain, because continuing would let a superseded source answer for a live one.
type ChainResolver []SecretResolver

// Redeem returns the first resolver's hit, or the LAST resolver's error so the message names the source the
// caller most likely meant.
func (c ChainResolver) Redeem(ref SecretRef) (string, error) {
	var lastErr error = fmt.Errorf("%w: %s", ErrUnknownSecret, ref)
	for _, r := range c {
		if r == nil {
			continue
		}
		v, err := r.Redeem(ref)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrUnknownSecret) {
			return "", err
		}
		lastErr = err
	}
	return "", lastErr
}

// Config constructs a Broker. Diagnostics, if set, receives non-secret routing
// breadcrumbs; the credential is never written to it.
type Config struct {
	Adapters    map[string]ModelAdapter
	Secrets     SecretResolver
	Diagnostics io.Writer
}

// Broker routes canonical requests to provider adapters and redeems credentials.
type Broker struct {
	adapters map[string]ModelAdapter
	secrets  SecretResolver
	diag     io.Writer
}

// New builds a Broker. An absent resolver rejects every SecretRef.
func New(cfg Config) *Broker {
	secrets := cfg.Secrets
	if secrets == nil {
		secrets = StaticResolver{}
	}
	return &Broker{adapters: cfg.Adapters, secrets: secrets, diag: cfg.Diagnostics}
}

// Route executes req against the named provider. It is the sole executor: it
// redeems the request's SecretRef here and hands the value to the adapter, so the
// credential is confined to this call frame. It correlates the result to the
// request, records that exactly one attempt was made unless the adapter surfaced
// more (no hidden provider retry), and enforces the request's budget reservation.
func (b *Broker) Route(ctx context.Context, provider string, req Request, onDelta func(Delta)) (Result, error) {
	adapter, ok := b.adapters[provider]
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrUnknownProvider, provider)
	}
	if b.diag != nil {
		// A breadcrumb the executor ran — deliberately free of any credential.
		fmt.Fprintf(b.diag, "route provider=%s model_request_id=%s revision=%d step=%s\n",
			provider, req.ModelRequestID, req.RouteRevision, req.ModelStepID)
	}

	secret, err := b.secrets.Redeem(req.Secret)
	if err != nil {
		return Result{}, fmt.Errorf("redeem credential: %w", err)
	}

	res, err := adapter.Execute(ctx, req, secret, onDelta)
	if err != nil {
		return Result{}, err
	}

	res.ModelRequestID = req.ModelRequestID
	if res.Attempts == 0 {
		res.Attempts = 1
	}
	if err := req.Reservation.Admit(res.Usage); err != nil {
		return res, err
	}
	return res, nil
}
