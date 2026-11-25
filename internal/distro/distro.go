// Package distro は Kubernetes ディストリビューション (k3s / kubeadm / Talos) を抽象化する。
//
// Distro は「中で何を動かすか」だけを担当し、VM やコンテナの作り方 (Backend) は知らない。
package distro

import (
	"context"
	"net/netip"

	"github.com/sat0ken/niwakube/internal/pki"
)

// Role はノードの役割。
type Role string

const (
	RoleControlPlane Role = "control-plane"
	RoleWorker       Role = "worker"
)

// Secrets はクラスタの秘密情報。niwakubed が作って DB に保存する。
type Secrets struct {
	Token string                 `json:"token"`
	CAs   map[string]pki.KeyPair `json:"cas"`
	// ServiceAccountKey は ServiceAccount トークンの署名鍵 (PEM)。
	ServiceAccountKey string `json:"serviceAccountKey,omitempty"`
	// Extra はディストリ固有の秘密情報 (例: Talos の secrets bundle)。
	Extra map[string]string `json:"extra,omitempty"`
}

// Cluster は Distro がノードの設定を作るのに必要な、クラスタの情報。
type Cluster struct {
	Name    string
	Version string     // 例: k3s なら "v1.34.2+k3s1"。空ならディストリの既定 (stable)
	APIAddr netip.Addr // API サーバーのアドレス (いまは CP 1 台目。HA にしたら VIP)
	Secrets *Secrets
	Addons  Addons // CNI とコンテナランタイム
}

// Node は Distro が設定を作る対象のノード。
type Node struct {
	Name  string
	Role  Role
	Index int // 同じ役割の中での番号 (1 始まり)
	IP    netip.Addr
}

// Requirements は Distro が Backend に求めること。
type Requirements struct {
	NeedsOwnKernel bool     // Talos は true
	OnlyRuntimes   []string // 使えるコンテナランタイムが限られる場合 (Talos は runc のみ)。nil なら制限なし
}

// Distro は Kubernetes ディストリビューション。
type Distro interface {
	Name() string
	// DefaultVersion は Version が空のときに使う値 (表示用)。
	DefaultVersion() string
	ValidateVersion(v string) error
	Requirements() Requirements
	// Image はノードに使うイメージの論理名 (例: ubuntu-26.04、talos-v1.14.7)。version は正規化済みのバージョン。
	Image(version string) string
	// GenerateSecrets はクラスタの秘密情報を作る。
	GenerateSecrets(clusterName string) (*Secrets, error)
	// UserData はノードの cloud-init user-data に追加するキーを返す。
	UserData(c *Cluster, n *Node) (map[string]any, error)
	// AdminCAs は kubeconfig の発行に使う CA の名前 (Secrets.CAs のキー) を返す。
	// server はクライアントが API サーバーを検証する CA、client は admin 証明書に署名する CA。
	AdminCAs() (server, client string)
	// APIPort は API サーバーのポート。
	APIPort() int
}

// RawUserDataProvider は cloud-init ではなく、user-data をそのまま渡すディストリが実装する (Talos の machine config)。
// 実装していれば UserData の代わりに使う。
type RawUserDataProvider interface {
	RawUserData(c *Cluster, n *Node) ([]byte, error)
}

// Bootstrapper はノードを作ったあとに外から初期化の操作が必要なディストリが実装する (Talos の etcd の bootstrap)。
// 何度呼んでも安全であること (初期化済みなら nil を返す)。
type Bootstrapper interface {
	Bootstrap(ctx context.Context, c *Cluster) error
}

// ClientConfigProvider は Kubernetes 以外のクライアントの設定を発行できるディストリが実装する (Talos の talosconfig)。
type ClientConfigProvider interface {
	ClientConfig(c *Cluster) (filename string, data []byte, err error)
}

// LocalStorage はノードのディスクを使う PV (local-path) の設定。
type LocalStorage struct {
	Builtin             bool   // ディストリに組み込まれている (k3s)。niwakubed は入れない
	Dir                 string // ノード上の保存先
	PrivilegedNamespace bool   // local-path の Namespace を Pod Security の privileged にする (Talos)
}

// LocalStorageProvider は local-path の設定がディストリごとに違う場合に実装する。
// 実装しなければ /opt/local-path-provisioner に niwakubed が入れる。
type LocalStorageProvider interface {
	LocalStorage() LocalStorage
}
