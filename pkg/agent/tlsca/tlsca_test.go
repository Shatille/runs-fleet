package tlsca

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestNewCAIsCertificateAuthority(t *testing.T) {
	t.Parallel()

	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if !ca.cert.IsCA {
		t.Error("CA cert IsCA = false")
	}
	if ca.cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA cert missing CertSign key usage")
	}
}

func TestIssueLeafVerifiesAgainstCAForSAN(t *testing.T) {
	t.Parallel()

	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := ca.IssueLeaf([]string{"results-receiver.actions.githubusercontent.com", "*.actions.githubusercontent.com"})
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	opts := x509.VerifyOptions{Roots: roots, DNSName: "results-receiver.actions.githubusercontent.com", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if _, err := leaf.Leaf.Verify(opts); err != nil {
		t.Errorf("leaf verify for exact SAN: %v", err)
	}

	// Wildcard SAN should cover a sibling host.
	wildOpts := x509.VerifyOptions{Roots: roots, DNSName: "foo.actions.githubusercontent.com", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if _, err := leaf.Leaf.Verify(wildOpts); err != nil {
		t.Errorf("leaf verify for wildcard SAN: %v", err)
	}
}

func TestIssueLeafRejectsUnlistedHost(t *testing.T) {
	t.Parallel()

	ca, _ := NewCA()
	leaf, err := ca.IssueLeaf([]string{"results-receiver.actions.githubusercontent.com"})
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	opts := x509.VerifyOptions{Roots: roots, DNSName: "evil.example.com", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if _, err := leaf.Leaf.Verify(opts); err == nil {
		t.Error("leaf verified for an unlisted host, want failure")
	}
}

func TestIssueLeafRequiresSAN(t *testing.T) {
	t.Parallel()

	ca, _ := NewCA()
	if _, err := ca.IssueLeaf(nil); err == nil {
		t.Error("IssueLeaf(nil) = nil error, want error")
	}
}

func TestWriteCertPEM(t *testing.T) {
	t.Parallel()

	ca, _ := NewCA()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := ca.WriteCertPEM(path); err != nil {
		t.Fatalf("WriteCertPEM: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written cert: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		t.Error("written PEM is not a parseable certificate")
	}
}
