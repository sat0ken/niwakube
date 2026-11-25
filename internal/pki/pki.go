// Package pki はクラスタの CA と証明書を作る。
//
// 秘密情報はすべて niwakubed 側で先に作る方針 (docs/design.md)。
// こうすると kubeconfig を自分で発行でき、VM に SSH して取りに行く必要がない。
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// KeyPair は PEM 形式の証明書と秘密鍵。
type KeyPair struct {
	CertPEM string `json:"cert"`
	KeyPEM  string `json:"key"`
}

// CAValidity は CA の有効期間 (自宅ラボなので長めにとる)。
const CAValidity = 10 * 365 * 24 * time.Hour

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func encodeECKey(k *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

// NewCA は自己署名の CA (ECDSA P-256) を作る。
func NewCA(commonName string) (KeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	sn, err := serial()
	if err != nil {
		return KeyPair{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return KeyPair{}, err
	}
	keyPEM, err := encodeECKey(key)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:  keyPEM,
	}, nil
}

// Parse は証明書と秘密鍵を読む。
func (kp KeyPair) Parse() (*x509.Certificate, crypto.Signer, error) {
	cb, _ := pem.Decode([]byte(kp.CertPEM))
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, nil, errors.New("invalid certificate PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	kb, _ := pem.Decode([]byte(kp.KeyPEM))
	if kb == nil {
		return nil, nil, errors.New("invalid key PEM")
	}
	var key any
	switch kb.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(kb.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(kb.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(kb.Bytes)
	default:
		return nil, nil, fmt.Errorf("unsupported key type %q", kb.Type)
	}
	if err != nil {
		return nil, nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("key is not a signer")
	}
	return cert, signer, nil
}

// SignClient はこの CA でクライアント証明書を発行する。
// Kubernetes は CN をユーザー名、O をグループとして扱う。
func (kp KeyPair) SignClient(commonName string, orgs []string, validity time.Duration) (KeyPair, error) {
	caCert, caKey, err := kp.Parse()
	if err != nil {
		return KeyPair{}, fmt.Errorf("parse CA: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	sn, err := serial()
	if err != nil {
		return KeyPair{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: commonName, Organization: orgs},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, key.Public(), caKey)
	if err != nil {
		return KeyPair{}, err
	}
	keyPEM, err := encodeECKey(key)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:  keyPEM,
	}, nil
}

// NewRSAKey は RSA の秘密鍵を PKCS#1 PEM で返す (ServiceAccount トークンの署名鍵などに使う)。
func NewRSAKey(bits int) (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), nil
}

// RandomToken は 16 進のランダムな文字列を返す (bytes バイト分)。
func RandomToken(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
