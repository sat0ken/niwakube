package ch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client は Cloud Hypervisor の REST API (Unix socket) のクライアント。
// API の仕様: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
type Client struct {
	http *http.Client
}

func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// APIError は CH が 2xx 以外を返したときのエラー。
type APIError struct {
	Method, Path string
	StatusCode   int
	Body         string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloud-hypervisor %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost/api/v1/"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "vmm.ping", nil, nil)
}

func (c *Client) CreateVM(ctx context.Context, cfg *VmConfig) error {
	return c.do(ctx, http.MethodPut, "vm.create", cfg, nil)
}

func (c *Client) Boot(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "vm.boot", nil, nil)
}

func (c *Client) Info(ctx context.Context) (*VmInfo, error) {
	var info VmInfo
	if err := c.do(ctx, http.MethodGet, "vm.info", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// PowerButton は ACPI の電源ボタンを押す (ゲストに正常シャットダウンを依頼する)。
func (c *Client) PowerButton(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "vm.power-button", nil, nil)
}

// ShutdownVMM は VMM プロセス自体を終了させる。
func (c *Client) ShutdownVMM(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "vmm.shutdown", nil, nil)
}

// --- API の型 (使う部分だけ) ---

type VmConfig struct {
	Cpus    CpusConfig     `json:"cpus"`
	Memory  MemoryConfig   `json:"memory"`
	Payload PayloadConfig  `json:"payload"`
	Disks   []DiskConfig   `json:"disks,omitempty"`
	Net     []NetConfig    `json:"net,omitempty"`
	Rng     *RngConfig     `json:"rng,omitempty"`
	Serial  *ConsoleConfig `json:"serial,omitempty"`
	Console *ConsoleConfig `json:"console,omitempty"`
}

type CpusConfig struct {
	BootVcpus int `json:"boot_vcpus"`
	MaxVcpus  int `json:"max_vcpus"`
}

type MemoryConfig struct {
	Size int64 `json:"size"` // バイト
}

type PayloadConfig struct {
	Firmware  string `json:"firmware,omitempty"`
	Kernel    string `json:"kernel,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
	Initramfs string `json:"initramfs,omitempty"`
}

type DiskConfig struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
	// ImageType は "Raw" | "Qcow2" | "Vhdx" | "FixedVhd" | "FlatVmdk"。v55 以降は必須。
	ImageType string `json:"image_type,omitempty"`
}

type NetConfig struct {
	Tap string `json:"tap,omitempty"`
	Mac string `json:"mac,omitempty"`
}

type RngConfig struct {
	Src string `json:"src"`
}

// ConsoleConfig の Mode は "Off" | "Pty" | "Tty" | "File" | "Socket" | "Null"。
type ConsoleConfig struct {
	Mode string `json:"mode"`
	File string `json:"file,omitempty"`
}

// VmInfo の State は "Created" | "Running" | "Shutdown" | "Paused"。
type VmInfo struct {
	Config VmConfig `json:"config"`
	State  string   `json:"state"`
}
