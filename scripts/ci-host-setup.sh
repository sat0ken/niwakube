#!/usr/bin/env bash
# GitHub Actions のランナーをホストとして準備する (E2E 用)。
#   1. /dev/kvm を使えるようにする
#   2. scripts/host-setup.sh を実行する
#   3. Docker が FORWARD を DROP にしているので nkbr0 の転送を許可する
set -euo pipefail

cd "$(dirname "$0")/.."

echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' | sudo tee /etc/udev/rules.d/99-kvm4all.rules >/dev/null
sudo udevadm control --reload-rules
sudo udevadm trigger --name-match=kvm
echo "--- runner"
lscpu | grep -E '^(Model name|CPU\(s\)|Virtualization|Hypervisor vendor)' || true
free -h
ls -l /dev/kvm

# Talos のイメージは niwakubed に組み込んだ machinery と同じバージョンにする
TALOS_VERSIONS=$(go list -m -f '{{.Version}}' github.com/siderolabs/talos/pkg/machinery)
sudo TALOS_VERSIONS="$TALOS_VERSIONS" ./scripts/host-setup.sh

if sudo iptables -nL DOCKER-USER >/dev/null 2>&1; then
  sudo iptables -I DOCKER-USER -i nkbr0 -j ACCEPT
  sudo iptables -I DOCKER-USER -o nkbr0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
fi
