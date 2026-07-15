#!/bin/bash
# Build and deploy opencode-go-proxy as a user systemd service.
set -euo pipefail

cd "$(dirname "$0")"

echo "=== Building opencode-go-proxy ==="
go build -o opencode-go-proxy .

echo "=== Stopping old service ==="
systemctl --user stop opencode-go-proxy.service 2>/dev/null || true
sleep 0.5

echo "=== Installing binary ==="
cp opencode-go-proxy ~/.local/bin/opencode-go-proxy

echo "=== Installing systemd unit ==="
cp opencode-go-proxy.service ~/.config/systemd/user/opencode-go-proxy.service

echo "=== Reloading systemd ==="
systemctl --user daemon-reload

echo "=== Starting service ==="
systemctl --user start opencode-go-proxy.service

echo "=== Status ==="
sleep 2
systemctl --user status opencode-go-proxy.service --no-pager

echo ""
echo "Done. Check: curl http://localhost:8082/health"