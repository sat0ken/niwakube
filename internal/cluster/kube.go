package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/pki"
)

// Prober はクラスタの API に問い合わせて、Ready なノードの数を返す。
type Prober interface {
	Probe(ctx context.Context, endpoint, serverCA string, admin pki.KeyPair) (ready, total int, err error)
}

// HTTPProber は client-go を使わず、素の HTTP で /api/v1/nodes を読む。
type HTTPProber struct{}

func (HTTPProber) Probe(ctx context.Context, endpoint, serverCA string, admin pki.KeyPair) (int, int, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(serverCA)) {
		return 0, 0, errors.New("invalid server CA")
	}
	cert, err := tls.X509KeyPair([]byte(admin.CertPEM), []byte(admin.KeyPEM))
	if err != nil {
		return 0, 0, err
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/nodes", nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, 0, fmt.Errorf("GET /api/v1/nodes: HTTP %d: %s", resp.StatusCode, b)
	}
	var list struct {
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return 0, 0, err
	}
	ready := 0
	for _, n := range list.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
			}
		}
	}
	return ready, len(list.Items), nil
}

type kubeconfig struct {
	APIVersion     string `yaml:"apiVersion"`
	Kind           string `yaml:"kind"`
	Clusters       []kcCluster
	Users          []kcUser
	Contexts       []kcContext
	CurrentContext string `yaml:"current-context"`
}

type kcCluster struct {
	Name    string `yaml:"name"`
	Cluster struct {
		Server                   string `yaml:"server"`
		CertificateAuthorityData string `yaml:"certificate-authority-data"`
		ProxyURL                 string `yaml:"proxy-url,omitempty"`
	} `yaml:"cluster"`
}

type kcUser struct {
	Name string `yaml:"name"`
	User struct {
		ClientCertificateData string `yaml:"client-certificate-data"`
		ClientKeyData         string `yaml:"client-key-data"`
	} `yaml:"user"`
}

type kcContext struct {
	Name    string `yaml:"name"`
	Context struct {
		Cluster string `yaml:"cluster"`
		User    string `yaml:"user"`
	} `yaml:"context"`
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func buildKubeconfig(name, server, serverCA, proxyURL string, admin pki.KeyPair) ([]byte, error) {
	user := name + "-admin"
	var c kcCluster
	c.Name = name
	c.Cluster.Server = server
	c.Cluster.CertificateAuthorityData = b64(serverCA)
	c.Cluster.ProxyURL = proxyURL
	var u kcUser
	u.Name = user
	u.User.ClientCertificateData = b64(admin.CertPEM)
	u.User.ClientKeyData = b64(admin.KeyPEM)
	var ctx kcContext
	ctx.Name = name
	ctx.Context.Cluster = name
	ctx.Context.User = user
	return yaml.Marshal(kubeconfig{
		APIVersion: "v1", Kind: "Config",
		Clusters: []kcCluster{c}, Users: []kcUser{u}, Contexts: []kcContext{ctx},
		CurrentContext: name,
	})
}
