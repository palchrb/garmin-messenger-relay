#!/usr/bin/env bash
# Idempotent installer for running garmin-messenger-relay as a systemd service.
#
# Usage:
#   sudo ./install.sh                      # build from this checkout (needs Go)
#   sudo ./install.sh --binary ./relay     # install a downloaded release binary
#   sudo ./install.sh --version v1.2.3     # download that release from GitHub
#
# Safe to re-run: existing config and session data are never overwritten,
# the binary and unit file are updated in place, and the service is
# restarted only if it was already running.
set -euo pipefail

NAME=garmin-messenger-relay
USER_NAME=${RELAY_USER:-garmin-relay}
INSTALL_DIR=${INSTALL_DIR:-/opt/$NAME}
UNIT=/etc/systemd/system/$NAME.service
REPO=palchrb/garmin-messenger-relay
SRC_DIR=$(cd "$(dirname "$0")" && pwd)

BINARY=""
VERSION=""
while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BINARY=$2; shift 2 ;;
    --version) VERSION=$2; shift 2 ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "Run as root (sudo $0)" >&2
  exit 1
fi
if ! command -v systemctl >/dev/null; then
  echo "systemd not found; this installer is for systemd hosts" >&2
  exit 1
fi

log() { printf '==> %s\n' "$*"; }

# 1. Obtain a binary.
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
if [ -n "$BINARY" ]; then
  log "Using binary $BINARY"
  cp "$BINARY" "$TMP/$NAME"
elif [ -n "$VERSION" ]; then
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "Unsupported architecture $ARCH; pass --binary" >&2; exit 1 ;;
  esac
  ASSET="$NAME-linux-$ARCH"
  URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
  log "Downloading $URL"
  curl -fsSL -o "$TMP/$NAME" "$URL"
  curl -fsSL -o "$TMP/SHA256SUMS.txt" "https://github.com/$REPO/releases/download/$VERSION/SHA256SUMS.txt"
  (cd "$TMP" && grep " $ASSET\$" SHA256SUMS.txt | sed "s/$ASSET/$NAME/" | sha256sum -c -)
elif command -v go >/dev/null && [ -f "$SRC_DIR/go.mod" ]; then
  log "Building from source in $SRC_DIR"
  COMMIT=$(git -C "$SRC_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)
  (cd "$SRC_DIR" && CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.Version=$(git -C "$SRC_DIR" describe --tags --always 2>/dev/null || echo dev) -X main.Commit=$COMMIT" \
    -o "$TMP/$NAME" ./cmd/$NAME)
else
  echo "No Go toolchain and no --binary/--version given" >&2
  exit 1
fi
chmod 0755 "$TMP/$NAME"

# 2. Service user and directories.
if ! id "$USER_NAME" >/dev/null 2>&1; then
  log "Creating system user $USER_NAME"
  useradd --system --shell /usr/sbin/nologin --home-dir "$INSTALL_DIR" --no-create-home "$USER_NAME"
fi
install -d -m 0750 -o "$USER_NAME" -g "$USER_NAME" "$INSTALL_DIR" "$INSTALL_DIR/sessions"

# 3. Binary (replace atomically so a running service keeps its old inode).
log "Installing binary to $INSTALL_DIR/$NAME"
install -m 0755 -o root -g root "$TMP/$NAME" "$INSTALL_DIR/$NAME.new"
mv -f "$INSTALL_DIR/$NAME.new" "$INSTALL_DIR/$NAME"

# 4. Config: created once, never overwritten.
if [ ! -f "$INSTALL_DIR/config.yaml" ]; then
  log "Writing example config to $INSTALL_DIR/config.yaml"
  (cd "$INSTALL_DIR" && "$INSTALL_DIR/$NAME" init -config "$INSTALL_DIR/config.yaml" >/dev/null)
  chown "$USER_NAME:$USER_NAME" "$INSTALL_DIR/config.yaml"
  chmod 0600 "$INSTALL_DIR/config.yaml"
  NEW_CONFIG=1
else
  NEW_CONFIG=0
fi

# 5. Unit file.
log "Installing systemd unit $UNIT"
cat > "$UNIT" <<UNITEOF
[Unit]
Description=Garmin Messenger Relay
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$USER_NAME
Group=$USER_NAME
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/$NAME run -config $INSTALL_DIR/config.yaml
# The relay exits non-zero on purpose when Garmin refuses to reconnect.
Restart=always
RestartSec=10

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$INSTALL_DIR
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
UNITEOF
systemctl daemon-reload
systemctl enable "$NAME" >/dev/null

# 6. Start or restart.
if [ "$NEW_CONFIG" = 1 ]; then
  cat <<MSG

Installed. Before starting the service:

  1. Edit $INSTALL_DIR/config.yaml (phone number, SMTP, IMAP, recipients)
  2. Log in to Garmin once, as the service user:
       sudo -u $USER_NAME $INSTALL_DIR/$NAME login -config $INSTALL_DIR/config.yaml
  3. Check everything:
       sudo -u $USER_NAME $INSTALL_DIR/$NAME status -config $INSTALL_DIR/config.yaml
  4. Start it:
       sudo systemctl start $NAME
       sudo journalctl -u $NAME -f
MSG
elif systemctl is-active --quiet "$NAME"; then
  log "Restarting $NAME"
  systemctl restart "$NAME"
  systemctl --no-pager --lines=0 status "$NAME" || true
else
  log "Service installed and enabled; start it with: sudo systemctl start $NAME"
fi
