#!/usr/bin/env bash
# xdev upgrader — pull the latest published release and install it, on a machine
# that has only the binary: no checkout, no Go, no installer on disk.
#
#   curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/upgrade.sh | sudo bash
#   curl -fsSL .../upgrade.sh | sudo bash -s -- --version v0.2.7   # pin a release
#   curl -fsSL .../upgrade.sh | sudo bash -s -- --check            # look, don't touch
#
# This is the bootstrap. Once a binary with `xdev update` is installed, that
# subcommand does the same job with no network fetch of a script — and this
# script hands over to it when it is available, so the tested path is the one
# that normally runs. Use deploy/install.sh for a first install, and
# deploy/update.sh to install a build from a checkout.
#
# Config (/etc/xdev/xdev.env), the sqlite database and your projects are never
# touched. The previous binary is kept, and a service that does not come back is
# rolled back automatically.
set -euo pipefail

# --- output helpers (same vocabulary as install.sh / update.sh) --------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_BLUE=$'\033[34m'; C_GREEN=$'\033[32m'
  C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'
else
  C_RESET=""; C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_RED=""
fi
info() { printf "%s▸%s %s\n" "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf "%s✓%s %s\n" "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf "%s!%s %s\n" "$C_YELLOW" "$C_RESET" "$*" >&2; }
err()  { printf "%s✗%s %s\n" "$C_RED" "$C_RESET" "$*" >&2; }
die()  { err "$*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
as_root() { if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi; }

XDEV_REPO="${XDEV_REPO:-leyume/xdev}"
VERSION="${XDEV_VERSION:-latest}"
BIN_PATH="${XDEV_BIN_PATH:-}"
KEEP_BACKUPS=3
CHECK=0
NO_RESTART=0

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --bin-path) BIN_PATH="${2:-}"; shift 2 ;;
    --repo) XDEV_REPO="${2:-}"; shift 2 ;;
    --check) CHECK=1; shift ;;
    --no-restart) NO_RESTART=1; shift ;;
    -h|--help) sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

have curl || die "curl is required"

# --- 1. locate the installed binary ------------------------------------------
if [ -z "$BIN_PATH" ]; then
  BIN_PATH="$(command -v xdev 2>/dev/null || echo /usr/local/bin/xdev)"
fi
[ -x "$BIN_PATH" ] || die "no xdev at $BIN_PATH — use deploy/install.sh for a first install"
# Follow symlinks: renaming over a link would replace the link with a file and
# detach every other path pointing at the real binary.
if have readlink && readlink -f "$BIN_PATH" >/dev/null 2>&1; then
  BIN_PATH="$(readlink -f "$BIN_PATH")"
fi

CURRENT="$("$BIN_PATH" version 2>/dev/null | awk '{print $2}' || echo unknown)"
info "installed: ${CURRENT}  (${BIN_PATH})"

# --- 2. hand over to `xdev update` when the binary has it ---------------------
# One implementation of the risky part. Older binaries have no such subcommand
# and fall through to the shell path below, which is the only reason this script
# duplicates any of it.
if "$BIN_PATH" help 2>&1 | grep -q '^[[:space:]]*update[[:space:]]'; then
  info "this binary has \`xdev update\` — handing over to it"
  ARGS=(--version "$VERSION" --repo "$XDEV_REPO" --bin "$BIN_PATH")
  if [ "$CHECK" = 1 ]; then ARGS+=(--check); fi
  if [ "$NO_RESTART" = 1 ]; then ARGS+=(--no-restart); fi
  # exec cannot run a shell function, so the sudo choice is made inline. --check
  # writes nothing, so it must never prompt for a password — asking for root to
  # answer a question is how people learn to run the whole thing as root.
  if [ "$(id -u)" -eq 0 ] || [ "$CHECK" = 1 ]; then
    exec "$BIN_PATH" update "${ARGS[@]}"
  fi
  exec sudo "$BIN_PATH" update "${ARGS[@]}"
fi

# --- 3. detect platform -------------------------------------------------------
case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *) die "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU arch: $(uname -m)" ;;
esac
ASSET="xdev-${OS}-${ARCH}"

# --- 4. resolve the release ---------------------------------------------------
# The tag is read from the API rather than by following the /latest/download
# redirect, because it is what makes "already up to date" answerable at all.
if [ "$VERSION" = latest ]; then
  TAG="$(curl -fsSL -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/${XDEV_REPO}/releases/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$TAG" ] || die "could not ask GitHub for the latest release (rate limited, or no releases published)"
else
  TAG="$VERSION"
fi
info "available: ${TAG}"

if [ "$TAG" = "$CURRENT" ]; then
  ok "already up to date"
  exit 0
fi
if [ "$CHECK" = 1 ]; then
  ok "an upgrade is available: ${CURRENT} → ${TAG}"
  exit 0
fi

# --- 5. download and verify ---------------------------------------------------
BASE="https://github.com/${XDEV_REPO}/releases/download/${TAG}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

info "downloading ${ASSET} (${TAG})…"
curl -fsSL "${BASE}/${ASSET}" -o "${TMP}/${ASSET}" \
  || die "no ${ASSET} in release ${TAG}"

sum_of() {
  if have sha256sum; then sha256sum "$1" | awk '{print $1}'
  elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
  fi
}
if curl -fsSL "${BASE}/checksums.txt" -o "${TMP}/checksums.txt" 2>/dev/null; then
  WANT="$(awk -v a="$ASSET" '$2==a || $2=="*"a {print $1}' "${TMP}/checksums.txt" | head -n1)"
  GOT="$(sum_of "${TMP}/${ASSET}")"
  if [ -z "$WANT" ] || [ -z "$GOT" ]; then
    warn "could not verify the download (no entry for ${ASSET}, or no sha256 tool)"
  elif [ "$WANT" != "$GOT" ]; then
    die "checksum mismatch for ${ASSET} (want ${WANT}, got ${GOT}) — refusing to install"
  else
    ok "checksum verified"
  fi
else
  warn "release ${TAG} publishes no checksums.txt — installing unverified"
fi

chmod +x "${TMP}/${ASSET}"
NEW="$("${TMP}/${ASSET}" version 2>/dev/null | awk '{print $2}')"
[ -n "$NEW" ] || die "the downloaded binary does not run here — wrong OS/arch?"

# --- 6. back up, then swap atomically ----------------------------------------
BACKUP="${BIN_PATH}.$(date +%Y%m%d%H%M%S).bak"
as_root cp -p "$BIN_PATH" "$BACKUP"
ok "backed up → $BACKUP"

# Write beside the target, then rename: the running process keeps the old inode
# (no ETXTBSY) and there is no window where $BIN_PATH is missing or truncated.
as_root install -m 0755 "${TMP}/${ASSET}" "${BIN_PATH}.new"
as_root mv -f "${BIN_PATH}.new" "$BIN_PATH"
ok "installed ${NEW} → ${BIN_PATH}"

# --- 7. restart, and roll back if it does not come back ----------------------
SERVICE=""
if [ "$OS" = linux ] && have systemctl && systemctl list-unit-files xdev.service --no-legend 2>/dev/null | grep -q xdev; then
  SERVICE=systemd
elif [ "$OS" = darwin ] && have launchctl && as_root launchctl print system/com.leyume.xdev >/dev/null 2>&1; then
  SERVICE=launchd
fi

if [ -z "$SERVICE" ]; then
  warn "no xdev service is installed here — restart xdev yourself to pick this up"
  exit 0
fi
if [ "$NO_RESTART" = 1 ]; then
  ok "binary installed; leaving the service alone as asked"
  exit 0
fi

restart() {
  if [ "$SERVICE" = systemd ]; then as_root systemctl restart xdev
  else as_root launchctl kickstart -k system/com.leyume.xdev; fi
}
running() {
  if [ "$SERVICE" = systemd ]; then [ "$(systemctl is-active xdev 2>/dev/null)" = active ]
  else as_root launchctl print system/com.leyume.xdev >/dev/null 2>&1; fi
}
# The main process id, so a restart that never happened cannot pass for one that
# did. "Is it active?" is true either way when a restart fails outright — polkit
# declining an interactive authorisation is the usual cause — and then the old
# binary keeps serving under a green tick.
main_pid() {
  if [ "$SERVICE" = systemd ]; then
    systemctl show -p MainPID --value xdev 2>/dev/null || echo 0
  else
    as_root launchctl print system/com.leyume.xdev 2>/dev/null \
      | awk -F= '$1 ~ /^[[:space:]]*pid[[:space:]]*$/ {gsub(/[^0-9]/,"",$2); print $2; exit}'
  fi
}
# xdev opens sqlite, migrates and pushes proxy config before it serves, so poll
# rather than checking once: a needless rollback is worse than a slow success.
wait_restarted() {
  local prev="$1" i=0 pid
  while [ $i -lt 30 ]; do
    if running; then
      pid="$(main_pid)"
      # An unreadable pid is not evidence of anything; fall back to liveness
      # rather than failing an update over a missing property.
      if [ -z "$pid" ] || [ "$pid" = 0 ] || [ "$pid" != "$prev" ]; then return 0; fi
    fi
    sleep 1; i=$((i + 1))
  done
  return 1
}

PREV_PID="$(main_pid)"
info "restarting the service…"
RESTART_OK=1
restart || RESTART_OK=0
if wait_restarted "$PREV_PID"; then
  ok "xdev is up on ${NEW}"
elif [ "$RESTART_OK" = 0 ] && running; then
  # The restart never ran, so the binary is fine and rolling back would undo a
  # good update to work around a permissions problem.
  warn "the service is still running its previous process — ${NEW} is installed but not yet serving"
  die "could not restart xdev (run: sudo systemctl restart xdev)"
else
  err "the service did not come back — rolling back to ${CURRENT}"
  as_root install -m 0755 "$BACKUP" "${BIN_PATH}.new"
  as_root mv -f "${BIN_PATH}.new" "$BIN_PATH"
  ROLLBACK_PID="$(main_pid)"
  restart || true
  if wait_restarted "$ROLLBACK_PID"; then
    warn "rolled back; xdev is up on ${CURRENT}"
  else
    err "rollback did not start either — inspect the logs"
  fi
  if [ "$SERVICE" = systemd ]; then journalctl -u xdev -n 40 --no-pager || true; fi
  exit 1
fi

# --- 8. prune old backups -----------------------------------------------------
# shellcheck disable=SC2012  # names are ours and contain no newlines
ls -1t "${BIN_PATH}".*.bak 2>/dev/null | tail -n +$((KEEP_BACKUPS + 1)) | while read -r old; do
  as_root rm -f "$old" && info "pruned $old"
done

ok "upgrade complete — ${CURRENT} → ${NEW}"
