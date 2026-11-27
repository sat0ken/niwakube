BIN := bin/niwakubed
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet install e2e e2e-cluster

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/niwakubed
	CGO_ENABLED=0 go build -trimpath -o $(dir $(BIN))termcheck ./scripts/termcheck

test:
	go test -race ./...

vet:
	go vet ./...
	test -z "$$(gofmt -l .)"

install: build
	install -m 0755 $(BIN) /usr/local/bin/niwakubed
	install -m 0644 deploy/niwakubed.service /etc/systemd/system/niwakubed.service
	systemctl daemon-reload

# 実機 (または KVM の使える CI) で niwakubed の API を通しで試す。host-setup.sh を済ませておくこと
# バックエンド: cloud-hypervisor (VM) か nspawn。例: make e2e BACKEND=nspawn
BACKEND ?= cloud-hypervisor

e2e: build
	sudo BACKEND=$(BACKEND) ./scripts/e2e.sh $(BIN)

# クラスタを作る E2E。例: make e2e-cluster DISTRO=kubeadm CNI=cilium RUNTIME=crun RUNTIMES="crun youki"
DISTRO ?= k3s
CNI ?= cilium
RUNTIME ?= runc
RUNTIMES ?=
e2e-cluster: build
	sudo BACKEND=$(BACKEND) DISTRO=$(DISTRO) CNI=$(CNI) RUNTIME=$(RUNTIME) RUNTIMES="$(RUNTIMES)" LIFECYCLE="$(LIFECYCLE)" ./scripts/e2e-cluster.sh $(BIN)
