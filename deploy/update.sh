#!/usr/bin/env bash
# xdev updater — swap the running xdev binary for a newer build and restart the
# service, without touching config, data, or projects.
#
#   sudo deploy/update.sh                 # build this checkout, install, restart
#   sudo deploy/update.sh --binary ./xdev # install a binary you already built
#   sudo deploy/update.sh --no-build      # same, using ./xdev from the repo root
#   sudo deploy/update.sh --dry-run       # say what would happen, change nothing
#
# The swap is atomic (write beside, rename over) so the running process is never
# truncated, the previous binary is kept as a timestamped backup, and if the
# service fails to come back the old binary is put back and restarted. Only the
# binary moves — /etc/xdev/xdev.env, the SQLite data dir and your projects are
# left exactly as they are, so this is also how you roll forward a config-less
# release. Use install.sh, not this, for a first install.
set -euo pipefail

# --- output helpers (same vocabulary as install.sh) --------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_BLUE=$'\033[34m'; C_GREEN=$'\033[32m'
  C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'; C_BOLD=$'\033[1m'
else
  C_RESET=""; C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_BOLD=""
fi
info() { printf "%s▸%s %s\n" "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf "%s✓%s %s\n" "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf "%s!%s %s\n" "$C_YELLOW" "$C_RESET" "$*" >&2; }
err()  { printf "%s✗%s %s\n" "$C_RED" "$C_RESET" "$*" >&2; }
die()  { err "$*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
as_root() { if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi; }
# sha256 of a file, or "" when no hasher is available (then we install rather
# than guess). This is what decides whether there is anything to do: a version
# string can't tell two builds of the same dirty tree apart, and during
# development every build is one of those.
sum() {
  if have sha256sum; then sha256sum "$1" | cut -d' ' -f1
  elif have shasum; then shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

BIN_PATH="${XDEV_BIN_PATH:-/usr/local/bin/xdev}"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
KEEP_BACKUPS=3
SRC=""
BUILD=1
DRY_RUN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) SRC="${2:-}"; BUILD=0; shift 2 ;;
    --no-build) BUILD=0; shift ;;
    --bin-path) BIN_PATH="${2:-}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *) die "unsupported OS: $(uname -s)" ;;
esac

# --- 1. what is running now ---------------------------------------------------
[ -x "$BIN_PATH" ] || die "no xdev at $BIN_PATH — use deploy/install.sh for a first install"
CURRENT="$("$BIN_PATH" version 2>/dev/null || echo unknown)"
info "installed: $CURRENT"

# --- 2. the new binary --------------------------------------------------------
if [ "$BUILD" = 1 ]; then
  have go || die "go not found on PATH — build elsewhere and pass --binary <path>"
  [ -f "$REPO_DIR/go.mod" ] || die "$REPO_DIR is not the xdev checkout (no go.mod)"
  VERSION="$(cd "$REPO_DIR" && git describe --tags --always --dirty 2>/dev/null || echo dev)"
  SRC="$(mktemp -d)/xdev"
  info "building $VERSION from $REPO_DIR…"
  # Build as the invoking user, not root: root's PATH often has no go, and the
  # module cache belongs to the user.
  if [ "$(id -u)" -eq 0 ] && [ -n "${SUDO_USER:-}" ]; then
    chown "$SUDO_USER" "$(dirname "$SRC")"
    sudo -u "$SUDO_USER" env PATH="$PATH" HOME="$(eval echo "~$SUDO_USER")" \
      sh -c "cd '$REPO_DIR' && CGO_ENABLED=0 go build -ldflags '-X main.version=$VERSION' -o '$SRC' ./cmd/xdev"
  else
    (cd "$REPO_DIR" && CGO_ENABLED=0 go build -ldflags "-X main.version=$VERSION" -o "$SRC" ./cmd/xdev)
  fi
else
  [ -n "$SRC" ] || SRC="$REPO_DIR/xdev"
fi
[ -f "$SRC" ] || die "no binary at $SRC"
chmod +x "$SRC" 2>/dev/null || true
NEW="$("$SRC" version 2>/dev/null || die "$SRC does not run here — wrong OS/arch?")"
info "new:       $NEW"

# Same bytes, not just the same name: two builds of a dirty working tree carry
# the same `git describe` version, so comparing the strings would report
# "nothing to do" for every rebuild between commits — the one case this script
# exists for during development.
CUR_SUM="$(sum "$BIN_PATH")"
NEW_SUM="$(sum "$SRC")"
if [ -n "$NEW_SUM" ] && [ "$NEW_SUM" = "$CUR_SUM" ]; then
  ok "already running this exact binary ($NEW) — nothing to do"
  exit 0
fi
if [ "$NEW" = "$CURRENT" ]; then
  info "same version string, different build — installing it"
fi

if [ "$DRY_RUN" = 1 ]; then
  ok "dry run: would install $SRC → $BIN_PATH and restart the service"
  exit 0
fi

# --- 3. back up, then swap atomically ----------------------------------------
STAMP="$(date +%Y%m%d%H%M%S)"
BACKUP="${BIN_PATH}.${STAMP}.bak"
as_root cp -p "$BIN_PATH" "$BACKUP"
ok "backed up → $BACKUP"

# install writes a fresh inode beside the target, then rename() swaps it in one
# step: the running process keeps the old inode until it exits (no ETXTBSY, no
# window where $BIN_PATH is missing or half-written).
as_root install -m 0755 "$SRC" "${BIN_PATH}.new"
as_root mv -f "${BIN_PATH}.new" "$BIN_PATH"
ok "installed $NEW → $BIN_PATH"

# --- 4. restart the service ---------------------------------------------------
restart() {
  if [ "$OS" = linux ]; then
    as_root systemctl restart xdev
  else
    as_root launchctl kickstart -k system/com.leyume.xdev
  fi
}
running() {
  if [ "$OS" = linux ]; then
    [ "$(systemctl is-active xdev 2>/dev/null)" = active ]
  else
    as_root launchctl print system/com.leyume.xdev >/dev/null 2>&1
  fi
}

info "restarting the service…"
restart || true
sleep 3

if running; then
  ok "xdev is up on $("$BIN_PATH" version)"
else
  err "service did not come back — rolling back to $CURRENT"
  as_root install -m 0755 "$BACKUP" "${BIN_PATH}.new"
  as_root mv -f "${BIN_PATH}.new" "$BIN_PATH"
  restart || true
  sleep 3
  if running; then
    warn "rolled back; xdev is up on $("$BIN_PATH" version)"
  else
    err "rollback did not start either — inspect the logs"
  fi
  if [ "$OS" = linux ]; then
    journalctl -u xdev -n 40 --no-pager || true
  else
    tail -n 40 /usr/local/var/log/xdev.log || true
  fi
  exit 1
fi

# --- 5. prune old backups, keep the newest few -------------------------------
# shellcheck disable=SC2012  # names are ours and contain no newlines
ls -1t "${BIN_PATH}".*.bak 2>/dev/null | tail -n +$((KEEP_BACKUPS + 1)) | while read -r old; do
  as_root rm -f "$old" && info "pruned $old"
done

# --- 6. health ----------------------------------------------------------------
if [ "$OS" = linux ]; then
  ENV_PATH=/etc/xdev/xdev.env
  as_root systemctl --no-pager --lines=0 status xdev | head -3 || true
else
  ENV_PATH=/usr/local/etc/xdev/xdev.env
fi
# doctor reads the same env the service does, or it reports on the wrong data
# dir and invents problems.
as_root sh -c "set -a; [ -f $ENV_PATH ] && . $ENV_PATH; set +a; exec $BIN_PATH doctor" 2>&1 | tail -n 20 || true
ok "update complete — ${CURRENT} → ${NEW}"
