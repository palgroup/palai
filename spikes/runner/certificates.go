package runner

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"
)

type CertificateUsage uint8

const (
	CertificateUsageServer CertificateUsage = iota + 1
	CertificateUsageClient
)

type CertificateRequest struct {
	DNSName   string
	Usage     CertificateUsage
	NotBefore time.Time
	NotAfter  time.Time
}

type CertificateAuthority struct {
	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey
	pool        *x509.CertPool
}

func NewCertificateAuthority(now time.Time) (*CertificateAuthority, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Palai runner spike CA"},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return &CertificateAuthority{certificate: certificate, privateKey: privateKey, pool: pool}, nil
}

func (ca *CertificateAuthority) Issue(request CertificateRequest) (tls.Certificate, error) {
	if ca == nil || ca.certificate == nil || ca.privateKey == nil || ca.pool == nil {
		return tls.Certificate{}, errors.New("certificate authority is incomplete")
	}
	if request.DNSName == "" || request.NotBefore.IsZero() || !request.NotAfter.After(request.NotBefore) {
		return tls.Certificate{}, errors.New("DNS name and monotonic certificate lifetime are required")
	}
	if request.NotBefore.Before(ca.certificate.NotBefore) || request.NotAfter.After(ca.certificate.NotAfter) {
		return tls.Certificate{}, errors.New("leaf lifetime must be within CA lifetime")
	}
	var extendedUsage x509.ExtKeyUsage
	switch request.Usage {
	case CertificateUsageServer:
		extendedUsage = x509.ExtKeyUsageServerAuth
	case CertificateUsageClient:
		extendedUsage = x509.ExtKeyUsageClientAuth
		if request.NotAfter.Sub(request.NotBefore) > time.Minute {
			return tls.Certificate{}, errors.New("runner client certificate lifetime exceeds one minute")
		}
	default:
		return tls.Certificate{}, errors.New("unsupported certificate usage")
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: request.DNSName},
		DNSNames:     []string{request.DNSName},
		NotBefore:    request.NotBefore,
		NotAfter:     request.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{extendedUsage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &privateKey.PublicKey, ca.privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create leaf certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse leaf certificate: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der, ca.certificate.Raw},
		PrivateKey:  privateKey,
		Leaf:        leaf,
	}, nil
}

func NewControllerTLSConfig(
	certificate tls.Certificate,
	clientCA *CertificateAuthority,
	expectedRunnerDNS string,
	now time.Time,
) (*tls.Config, error) {
	if certificate.Leaf == nil || clientCA == nil || clientCA.pool == nil || expectedRunnerDNS == "" || now.IsZero() {
		return nil, errors.New("controller TLS identity, client CA, runner DNS and time are required")
	}
	configuration := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCA.pool.Clone(),
		NextProtos:   []string{"http/1.1"},
		Time:         func() time.Time { return now },
	}
	configuration.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
			return errors.New("runner certificate chain was not verified")
		}
		if !hasExactDNSIdentity(state.VerifiedChains[0][0], expectedRunnerDNS) {
			return errors.New("runner certificate DNS identity is not exact")
		}
		return nil
	}
	return configuration, nil
}

func NewRunnerTLSConfig(
	certificate *tls.Certificate,
	controllerCA *CertificateAuthority,
	expectedControllerDNS string,
	now time.Time,
) (*tls.Config, error) {
	if controllerCA == nil || controllerCA.pool == nil || expectedControllerDNS == "" || now.IsZero() {
		return nil, errors.New("controller CA, DNS identity and time are required")
	}
	configuration := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    controllerCA.pool.Clone(),
		ServerName: expectedControllerDNS,
		NextProtos: []string{"http/1.1"},
		Time:       func() time.Time { return now },
	}
	if certificate != nil {
		configuration.Certificates = []tls.Certificate{*certificate}
	}
	configuration.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
			return errors.New("controller certificate chain was not verified")
		}
		if !hasExactDNSIdentity(state.VerifiedChains[0][0], expectedControllerDNS) {
			return errors.New("controller certificate DNS identity is not exact")
		}
		return nil
	}
	return configuration, nil
}

func hasExactDNSIdentity(certificate *x509.Certificate, expected string) bool {
	return certificate != nil && len(certificate.DNSNames) == 1 && certificate.DNSNames[0] == expected
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	if serial.Sign() == 0 {
		return big.NewInt(1), nil
	}
	return serial, nil
}
