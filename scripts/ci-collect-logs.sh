#!/usr/bin/env bash
# E2E が失敗したときに、ホスト側のログを e2e-logs/ に集める (CI 用)。
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
sudo mkdir -p e2e-logs
for f in /var/lib/niwakube/vms/*/serial.log; do
  [ -f "$f" ] && sudo cp "$f" "e2e-logs/$(basename "$(dirname "$f")")-serial-host.log"
done
sudo journalctl --no-pager -u 'niwakube-vm-*' 2>&1 | sudo tee e2e-logs/journal-vms.txt >/dev/null
sudo chown -R "$USER" e2e-logs
ls -la e2e-logs
