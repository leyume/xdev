#!/usr/bin/env bash
# Uninstall xdev: stop + disable + remove the service and binary. Your data
# (the sqlite DB and generated project stacks) is KEPT unless you pass --purge.
#
#   sudo ./deploy/uninstall.sh            # remove service + binary, keep data
#   sudo ./deploy/uninstall.sh --purge    # also delete data dir + config
#
# It deliberately never touches running containers/projects beyond stopping the
# xdev service — your apps' containers and the projects/ stacks are left alone.
set -euo pipefail

PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_BLUE=$'\033[34m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'
else
  C_RESET=""; C_BLUE=""; C_GREEN=""; C_YELLOW=""
fi
info() { printf "%s▸%s %s\n" "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf "%s✓%s %s\n" "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf "%s!%s %s\n" "$C_YELLOW" "$C_RESET" "$*" >&2; }

as_root() { if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi; }

BIN_PATH=/usr/local/bin/xdev

case "$(uname -s)" in
  Linux)
    info "stopping + disabling systemd service…"
    as_root systemctl disable --now xdev 2>/dev/null || warn "xdev service not running"
    as_root rm -f /etc/systemd/system/xdev.service
    as_root systemctl daemon-reload || true
    DATA_DIR=/var/lib/xdev
    ENV_PATH=/etc/xdev/xdev.env
    ;;
  Darwin)
    info "removing LaunchDaemon…"
    PLIST=/Library/LaunchDaemons/com.leyume.xdev.plist
    as_root launchctl bootout system "$PLIST" 2>/dev/null || \
      as_root launchctl unload "$PLIST" 2>/dev/null || warn "LaunchDaemon not loaded"
    as_root rm -f "$PLIST"
    DATA_DIR=/var/lib/xdev
    [ -d "$DATA_DIR" ] || DATA_DIR="$HOME/Library/Application Support/xdev/data"
    ENV_PATH=/usr/local/etc/xdev/xdev.env
    [ -f "$ENV_PATH" ] || ENV_PATH="$HOME/Library/Application Support/xdev/xdev.env"
    ;;
  *) warn "unsupported OS; removing the binary only" ;;
esac

info "removing binary ${BIN_PATH}…"
as_root rm -f "$BIN_PATH"
ok "binary removed"

if [ "$PURGE" -eq 1 ]; then
  warn "purging data: ${DATA_DIR} and ${ENV_PATH}"
  as_root rm -rf "$DATA_DIR"
  as_root rm -f "$ENV_PATH"
  ok "data + config purged"
else
  info "kept data dir (${DATA_DIR}) and config (${ENV_PATH}); pass --purge to delete them"
fi
ok "xdev uninstalled"
