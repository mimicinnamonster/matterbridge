#!/usr/bin/env bash
# Deploy the freshly built Linux binary to the Oracle server and restart the
# existing systemd service (matterbridge.service is already installed/enabled).
#
# Usage:  ./deploy/deploy.sh [user@host] [--debug]
set -euo pipefail

SERVER="${1:-ubuntu@oracle}"
BIN="matterbridge"
DEBUG="${2:-}"

echo "==> Uploading binary"
scp "$BIN" "${SERVER}:~/matterbridge/matterbridge.new"

if [ "$DEBUG" = "--debug" ]; then
    echo "==> Installing debug drop-in (restarts with -debug)"
    ssh "$SERVER" 'sudo mkdir -p /etc/systemd/system/matterbridge.service.d && printf "[Service]\nExecStart=\nExecStart=/home/ubuntu/matterbridge/matterbridge -conf matterbridge.toml -debug\n" | sudo tee /etc/systemd/system/matterbridge.service.d/debug.conf > /dev/null && sudo systemctl daemon-reload'
else
    echo "==> Removing any debug drop-in"
    ssh "$SERVER" 'sudo rm -f /etc/systemd/system/matterbridge.service.d/debug.conf && sudo systemctl daemon-reload || true'
fi

echo "==> Replacing binary and restarting service"
ssh "$SERVER" 'cd ~/matterbridge && mv -f matterbridge.new matterbridge && chmod +x matterbridge && sudo systemctl restart matterbridge && echo done'

echo "==> Waiting for startup"
sleep 8

echo "==> Status"
ssh "$SERVER" 'systemctl status matterbridge --no-pager -l | head -15'

echo "==> Recent journal"
ssh "$SERVER" 'journalctl -u matterbridge -n 20 --no-pager'

echo "Deploy complete."