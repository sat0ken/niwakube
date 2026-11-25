package pki

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestCAAndClientCert(t *testing.T) {
	ca, err := NewCA("test-ca")
	if err != nil {
		t.Fatal(err)
	}
	caCert, _, err := ca.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if !caCert.IsCA || caCert.Subject.CommonName != "test-ca" {
		t.Fatalf("unexpected CA: %+v", caCert.Subject)
	}

	client, err := ca.SignClient("admin", []string{"system:masters"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := client.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "admin" || cert.Subject.Organization[0] != "system:masters" || cert.IsCA {
		t.Fatalf("unexpected client cert: %+v", cert.Subject)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("client cert does not verify against CA: %v", err)
	}
}

func TestRSAKeyAndToken(t *testing.T) {
	k, err := NewRSAKey(2048)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(k))
	if b == nil || b.Type != "RSA PRIVATE KEY" {
		t.Fatal("bad RSA PEM")
	}
	tok, err := RandomToken(16)
	if err != nil || len(tok) != 32 {
		t.Fatalf("token %q, %v", tok, err)
	}
}
