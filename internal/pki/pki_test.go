package pki

import (
	"crypto/x509"
	"encoding/pem"
	"regexp"
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

func TestKubeadmHelpers(t *testing.T) {
	priv, err := NewRSAKey(2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := PublicKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := pem.Decode([]byte(pub)); b == nil || b.Type != "PUBLIC KEY" {
		t.Fatalf("bad public key PEM: %s", pub)
	}

	ca, err := NewCA("ca")
	if err != nil {
		t.Fatal(err)
	}
	h, err := CACertHash(ca.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(h) {
		t.Errorf("hash %q", h)
	}

	tok, err := BootstrapToken()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`).MatchString(tok) {
		t.Errorf("token %q", tok)
	}
}
