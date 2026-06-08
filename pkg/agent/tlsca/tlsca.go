// Package tlsca mints a per-instance, ephemeral certificate authority used by
// the on-host cache interceptor to terminate TLS for the GitHub Actions results
// host. The CA private key is generated at boot and never leaves the runner, so
// a compromise is bounded to a single ephemeral instance — unlike a CA baked
// into the AMI, whose key would be shared across the whole fleet.
package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// caValidity bounds the CA and leaf lifetime. Runner instances are ephemeral
// and short-lived; a day is ample and avoids long-lived trust material.
const caValidity = 24 * time.Hour

// CA is an in-memory certificate authority that issues leaf certificates for
// the hosts the interceptor terminates.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

// NewCA generates a fresh ephemeral CA.
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "runs-fleet runner cache CA", Organization: []string{"runs-fleet"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	return &CA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

// CertPEM returns the CA certificate in PEM form, for installation into the
// system trust store and NODE_EXTRA_CA_CERTS.
func (ca *CA) CertPEM() []byte {
	out := make([]byte, len(ca.certPEM))
	copy(out, ca.certPEM)
	return out
}

// WriteCertPEM writes the CA certificate to path, world-readable so non-root
// job processes (Node-based actions) can load it.
func (ca *CA) WriteCertPEM(path string) error {
	if err := os.WriteFile(path, ca.certPEM, 0o644); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}
	return nil
}

// IssueLeaf issues a server certificate valid for the given SANs (DNS names or
// IPs), signed by the CA. The returned certificate is ready for tls.Config.
func (ca *CA) IssueLeaf(sans []string) (tls.Certificate, error) {
	if len(sans) == 0 {
		return tls.Certificate{}, fmt.Errorf("at least one SAN is required")
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: sans[0]},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(caValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &leafKey.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create leaf certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse leaf certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: leafKey, Leaf: leaf}, nil
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return serial, nil
}
