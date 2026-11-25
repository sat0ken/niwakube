package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sat0ken/niwakube/internal/pki"
)

// TestHTTPProber は本物の TLS (サーバー証明書の検証 + クライアント証明書認証) で Probe を確かめる。
func TestHTTPProber(t *testing.T) {
	serverCA, err := pki.NewCA("server-ca")
	if err != nil {
		t.Fatal(err)
	}
	clientCA, err := pki.NewCA("client-ca")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := clientCA.SignClient("niwakube-admin", []string{"system:masters"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// API サーバーのふり: 127.0.0.1 の SAN を持つ証明書で待ち受け、client-ca のクライアント証明書を要求する
	caCert, caKey, _ := serverCA.Parse()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kube-apiserver"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientPool := x509.NewCertPool()
	clientPool.AppendCertsFromPEM([]byte(clientCA.CertPEM))

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes" || r.TLS.PeerCertificates[0].Subject.Organization[0] != "system:masters" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"items":[
			{"status":{"conditions":[{"type":"Ready","status":"True"}]}},
			{"status":{"conditions":[{"type":"Ready","status":"False"}]}},
			{"status":{"conditions":[{"type":"MemoryPressure","status":"False"},{"type":"Ready","status":"True"}]}}]}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientPool,
	}
	srv.StartTLS()
	defer srv.Close()

	ready, total, err := HTTPProber{}.Probe(context.Background(), srv.URL, serverCA.CertPEM, admin)
	if err != nil {
		t.Fatal(err)
	}
	if ready != 2 || total != 3 {
		t.Errorf("ready=%d total=%d", ready, total)
	}

	// 別の CA を信頼していると失敗する (サーバー証明書をちゃんと検証している)
	other, _ := pki.NewCA("other")
	if _, _, err := (HTTPProber{}).Probe(context.Background(), srv.URL, other.CertPEM, admin); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("expected certificate error, got %v", err)
	}
}
