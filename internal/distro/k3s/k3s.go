// Package k3s は k3s のクラスタを作る Distro。
//
// CA は niwakubed が作り、k3s を初めて起動する前に /var/lib/rancher/k3s/server/tls に置く
// (k3s の「カスタム CA 証明書」の仕組み)。こうすると niwakubed が admin の kubeconfig を自分で発行できる。
// 参考: https://docs.k3s.io/cli/certificate#using-custom-ca-certificates
package k3s

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/sat0ken/niwakube/internal/distro"
	"github.com/sat0ken/niwakube/internal/pki"
)

const Name = "k3s"

const tlsDir = "/var/lib/rancher/k3s/server/tls"

// k3s がカスタム CA として読むファイル (Secrets.CAs のキー → tlsDir からの相対パス)
var caFiles = map[string]string{
	"server-ca":         "server-ca",
	"client-ca":         "client-ca",
	"request-header-ca": "request-header-ca",
	"etcd-peer-ca":      "etcd/peer-ca",
	"etcd-server-ca":    "etcd/server-ca",
}

var versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+\+k3s\d+$`)

type K3s struct{}

var _ distro.Distro = K3s{}

func New() K3s { return K3s{} }

func (K3s) Name() string                      { return Name }
func (K3s) DefaultVersion() string            { return "stable" }
func (K3s) Requirements() distro.Requirements { return distro.Requirements{} }
func (K3s) Image() string                     { return "ubuntu-26.04" }
func (K3s) AdminCAs() (server, client string) { return "server-ca", "client-ca" }
func (K3s) APIPort() int                      { return 6443 }

func (K3s) ValidateVersion(v string) error {
	if v == "" || v == "stable" || v == "latest" || versionRe.MatchString(v) {
		return nil
	}
	return fmt.Errorf("k3s version must be like v1.34.2+k3s1, \"stable\" or \"latest\": %q", v)
}

func (K3s) GenerateSecrets(clusterName string) (*distro.Secrets, error) {
	s := &distro.Secrets{CAs: map[string]pki.KeyPair{}}
	var err error
	if s.Token, err = pki.RandomToken(32); err != nil {
		return nil, err
	}
	for name := range caFiles {
		if s.CAs[name], err = pki.NewCA(fmt.Sprintf("niwakube-%s-%s", clusterName, name)); err != nil {
			return nil, err
		}
	}
	if s.ServiceAccountKey, err = pki.NewRSAKey(2048); err != nil {
		return nil, err
	}
	return s, nil
}

type writeFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

func (k K3s) UserData(c *distro.Cluster, n *distro.Node) (map[string]any, error) {
	if c.Secrets == nil || c.Secrets.Token == "" {
		return nil, fmt.Errorf("cluster %s has no secrets", c.Name)
	}
	cfg := map[string]any{
		"token":   c.Secrets.Token,
		"node-ip": n.IP.String(),
	}
	var files []writeFile
	subcommand := "server"
	switch n.Role {
	case distro.RoleControlPlane:
		if n.Index != 1 {
			return nil, fmt.Errorf("k3s: only one control plane is supported for now")
		}
		cfg["tls-san"] = []string{n.IP.String(), c.APIAddr.String()}
		cfg["write-kubeconfig-mode"] = "0600"
		for name, rel := range caFiles {
			ca, ok := c.Secrets.CAs[name]
			if !ok {
				return nil, fmt.Errorf("k3s: CA %s is missing", name)
			}
			files = append(files,
				writeFile{Path: tlsDir + "/" + rel + ".crt", Permissions: "0644", Content: ca.CertPEM},
				writeFile{Path: tlsDir + "/" + rel + ".key", Permissions: "0600", Content: ca.KeyPEM},
			)
		}
		files = append(files, writeFile{Path: tlsDir + "/service.key", Permissions: "0600", Content: c.Secrets.ServiceAccountKey})
	case distro.RoleWorker:
		subcommand = "agent"
		cfg["server"] = fmt.Sprintf("https://%s:%d", c.APIAddr, k.APIPort())
	default:
		return nil, fmt.Errorf("unknown role %q", n.Role)
	}

	cfgYAML, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	files = append(files, writeFile{Path: "/etc/rancher/k3s/config.yaml", Permissions: "0600", Content: string(cfgYAML)})

	env := "INSTALL_K3S_CHANNEL=stable"
	switch v := c.Version; {
	case v == "latest" || v == "stable":
		env = "INSTALL_K3S_CHANNEL=" + v
	case v != "":
		env = "INSTALL_K3S_VERSION=" + v
	}
	install := fmt.Sprintf("curl -sfL https://get.k3s.io | %s sh -s - %s", env, subcommand)

	return map[string]any{
		"write_files": files,
		"runcmd":      []any{[]string{"sh", "-c", install}},
	}, nil
}
