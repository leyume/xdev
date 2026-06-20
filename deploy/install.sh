#!/usr/bin/env bash
# xdev installer — one command to install and run xdev on a fresh Ubuntu/Debian
# server or a local Mac:
#
#   curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | sudo bash
#
# It detects the OS + CPU arch, installs missing prerequisites (a container
# engine + Caddy), asks a few questions, downloads the matching prebuilt binary
# from a GitHub Release, verifies its checksum, writes config, installs a
# service, creates the admin account, and runs `xdev doctor`.
#
# Works both when run directly and when piped (`curl … | sudo bash`): every
# prompt is read from /dev/tty, and a fully non-interactive mode is available by
# pre-setting the XDEV_* env vars (set XDEV_NONINTERACTIVE=1).
#
# Key env knobs (see deploy/xdev.env.example for the runtime config vars):
#   XDEV_REPO=leyume/xdev          where to fetch the binary from
#   XDEV_VERSION=latest            a release tag (e.g. v0.1.0) or "latest"
#   XDEV_NONINTERACTIVE=1          never prompt; use env vars / defaults
#   XDEV_MODE=local|prod           drives the smart defaults below
set -euo pipefail

# --- output helpers ----------------------------------------------------------
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

# ask VAR "prompt" "default"  — resolves a value from (in order): an existing
# non-empty env var, a /dev/tty prompt, or the default (also used when
# non-interactive). The result is assigned back to VAR.
ask() {
  local __var="$1" __prompt="$2" __default="${3:-}" __cur __ans
  eval "__cur=\${$__var:-}"
  if [ -n "$__cur" ]; then return 0; fi               # already set via env
  if [ "${XDEV_NONINTERACTIVE:-0}" = "1" ] || [ ! -r /dev/tty ]; then
    eval "$__var=\$__default"; return 0
  fi
  if [ -n "$__default" ]; then
    printf "%s▸%s %s [%s]: " "$C_BLUE" "$C_RESET" "$__prompt" "$__default" > /dev/tty
  else
    printf "%s▸%s %s: " "$C_BLUE" "$C_RESET" "$__prompt" > /dev/tty
  fi
  IFS= read -r __ans < /dev/tty || __ans=""
  [ -z "$__ans" ] && __ans="$__default"
  eval "$__var=\$__ans"
}

# ask_secret VAR "prompt"  — hidden password entry with confirmation.
ask_secret() {
  local __var="$1" __prompt="$2" __cur __a __b
  eval "__cur=\${$__var:-}"
  if [ -n "$__cur" ]; then return 0; fi
  if [ "${XDEV_NONINTERACTIVE:-0}" = "1" ] || [ ! -r /dev/tty ]; then
    die "$__var must be set (non-interactive: no TTY for a password prompt)"
  fi
  while :; do
    printf "%s▸%s %s: " "$C_BLUE" "$C_RESET" "$__prompt" > /dev/tty
    IFS= read -rs __a < /dev/tty; echo > /dev/tty
    printf "%s▸%s %s (confirm): " "$C_BLUE" "$C_RESET" "$__prompt" > /dev/tty
    IFS= read -rs __b < /dev/tty; echo > /dev/tty
    if [ "$__a" != "$__b" ]; then warn "passwords didn't match — try again"; continue; fi
    if [ "${#__a}" -lt 8 ]; then warn "password must be at least 8 characters"; continue; fi
    break
  done
  eval "$__var=\$__a"
}

yesno() { # yesno VAR "prompt" "Y|N"  → sets VAR to "true"/"false"
  local __var="$1" __prompt="$2" __def="${3:-Y}" __cur __ans
  eval "__cur=\${$__var:-}"
  case "$__cur" in true|1|yes|on)  eval "$__var=true";  return 0;; esac
  case "$__cur" in false|0|no|off) eval "$__var=false"; return 0;; esac
  local hint="[Y/n]"; [ "$__def" = "N" ] && hint="[y/N]"
  if [ "${XDEV_NONINTERACTIVE:-0}" = "1" ] || [ ! -r /dev/tty ]; then
    [ "$__def" = "Y" ] && eval "$__var=true" || eval "$__var=false"; return 0
  fi
  printf "%s▸%s %s %s: " "$C_BLUE" "$C_RESET" "$__prompt" "$hint" > /dev/tty
  IFS= read -r __ans < /dev/tty || __ans=""
  [ -z "$__ans" ] && __ans="$__def"
  case "$__ans" in [Yy]*) eval "$__var=true";; *) eval "$__var=false";; esac
}

have() { command -v "$1" >/dev/null 2>&1; }

# prior_install reports (exit 0) whether xdev looks already installed on this
# host, so a re-run can upgrade in place instead of re-prompting for everything.
prior_install() {
  [ -f /usr/local/bin/xdev ] && return 0
  [ -f /etc/xdev/xdev.env ] && return 0
  [ -f /usr/local/etc/xdev/xdev.env ] && return 0
  [ -f "${HOME:-/nonexistent}/Library/Application Support/xdev/xdev.env" ] && return 0
  return 1
}
sha256_of() { # print the sha256 hex of a file, portably
  if have sha256sum; then sha256sum "$1" | awk '{print $1}';
  elif have shasum; then shasum -a 256 "$1" | awk '{print $1}';
  else die "no sha256 tool (sha256sum/shasum) available"; fi
}

# --- config (env-overridable) ------------------------------------------------
XDEV_REPO="${XDEV_REPO:-leyume/xdev}"
XDEV_VERSION="${XDEV_VERSION:-latest}"

# --- 1. detect OS + arch -----------------------------------------------------
detect_platform() {
  case "$(uname -s)" in
    Linux)  OS=linux ;;
    Darwin) OS=darwin ;;
    *) die "unsupported OS: $(uname -s) (xdev supports Linux and macOS)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "unsupported CPU arch: $(uname -m)" ;;
  esac
  ASSET="xdev-${OS}-${ARCH}"
  info "Detected: ${OS}/${ARCH}"
}

# --- 2. privilege ------------------------------------------------------------
ensure_privilege() {
  if [ "$OS" = linux ] && [ "$(id -u)" -ne 0 ]; then
    if have sudo; then
      warn "re-running with sudo (needs root for apt, ports 80/443, systemd, /etc/xdev)"
      exec sudo -E bash "$0" "$@"
    fi
    die "please run as root (Linux install needs apt, privileged ports, and systemd)"
  fi
  # macOS runs as the user; specific steps (caddy trust, LaunchDaemon, :443) ask
  # for sudo individually.
}

# run a command as root: direct if already root, else via sudo.
as_root() { if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi; }

# --- 3. prerequisites --------------------------------------------------------
install_engine() {
  if have "$ENGINE" && "$ENGINE" compose version >/dev/null 2>&1; then
    ok "$ENGINE present (compose ok)"; return 0
  fi
  info "installing $ENGINE…"
  if [ "$OS" = linux ]; then
    export DEBIAN_FRONTEND=noninteractive
    as_root apt-get update -qq
    if [ "$ENGINE" = docker ]; then
      as_root apt-get install -y -qq docker.io docker-compose-v2 || \
        die "could not install docker via apt (try https://get.docker.com)"
      as_root systemctl enable --now docker || true
    else
      as_root apt-get install -y -qq podman podman-compose || \
        die "could not install podman via apt"
    fi
  else # darwin
    have brew || die "Homebrew is required on macOS — install from https://brew.sh then re-run"
    if [ "$ENGINE" = docker ]; then
      brew install --cask docker || die "brew install docker failed"
      warn "Docker Desktop installed — launch it once so the daemon starts, then re-run if needed"
    else
      brew install podman || die "brew install podman failed"
      podman machine init 2>/dev/null || true
      podman machine start 2>/dev/null || true
    fi
  fi
  have "$ENGINE" && "$ENGINE" compose version >/dev/null 2>&1 \
    && ok "$ENGINE ready" || warn "$ENGINE installed but compose/daemon not confirmed — check later with: xdev doctor"
}

install_caddy() {
  if [ "$MANAGE_CADDY" != "true" ]; then
    info "skipping Caddy install (you'll manage Caddy yourself; XDEV_CADDY=false)"; return 0
  fi
  if have caddy; then ok "caddy present ($(caddy version 2>/dev/null | head -n1))"; return 0; fi
  info "installing caddy…"
  if [ "$OS" = linux ]; then
    export DEBIAN_FRONTEND=noninteractive
    as_root apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl gnupg
    curl -fsSL "https://dl.cloudsmith.io/public/caddy/stable/gpg.key" \
      | as_root gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -fsSL "https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt" \
      | as_root tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
    as_root apt-get update -qq && as_root apt-get install -y -qq caddy || die "could not install caddy"
  else
    have brew || die "Homebrew required to install caddy on macOS"
    brew install caddy || die "brew install caddy failed"
  fi
  have caddy && ok "caddy ready" || die "caddy still not on PATH after install"
}

# --- 4. interactive configuration -------------------------------------------
configure() {
  # Re-run / non-fresh box: detect an existing install so we upgrade in place
  # rather than re-prompting for (and possibly resetting) everything.
  EXISTING=0
  if prior_install; then
    EXISTING=1
    info "existing xdev install detected — upgrading in place (data preserved; config backed up)"
  fi

  # Mode drives the secure defaults.
  local engine_default=docker; [ "$OS" = darwin ] && engine_default=podman
  ask XDEV_MODE "Mode? [local/prod]" "local"
  ask XDEV_ENGINE "Container engine? [docker/podman]" "$engine_default"
  ENGINE="$XDEV_ENGINE"
  yesno XDEV_CADDY "Let xdev manage Caddy for you?" "Y"; MANAGE_CADDY="$XDEV_CADDY"

  if [ "$XDEV_MODE" = prod ]; then
    : "${XDEV_SECURE:=true}"; : "${XDEV_MANAGE_HOSTS:=false}"
    ask XDEV_BASE_DOMAIN "Primary base domain (e.g. apps.example.com)" ""
    [ -n "$XDEV_BASE_DOMAIN" ] || die "a base domain is required for prod mode (set XDEV_BASE_DOMAIN)"
    ask XDEV_ACME_EMAIL "Let's Encrypt email" ""
    [ -n "$XDEV_ACME_EMAIL" ] || die "a Let's Encrypt email is required for prod mode (set XDEV_ACME_EMAIL)"
    : "${XDEV_HTTPS_PORT:=443}"; : "${XDEV_HTTP_PORT:=80}"
  else
    : "${XDEV_SECURE:=false}"; : "${XDEV_MANAGE_HOSTS:=true}"
    ask XDEV_BASE_DOMAIN "Primary base domain (blank → .localhost)" ""
    : "${XDEV_HTTPS_PORT:=443}"; : "${XDEV_HTTP_PORT:=80}"
  fi

  ask XDEV_ADDR "Admin UI address" "127.0.0.1:7331"
  # Admin account: only prompt on a fresh install. On a re-run the existing admin
  # is kept (create-admin is idempotent anyway). Setting XDEV_ADMIN_EMAIL+PASSWORD
  # via env still lets an upgrade ensure/no-op the account.
  if [ "$EXISTING" = 1 ] && [ -z "${XDEV_ADMIN_EMAIL:-}" ]; then
    info "keeping the existing admin account (add another later with: xdev create-admin <email>)"
  else
    ask XDEV_ADMIN_EMAIL "Admin email" ""
    [ -n "$XDEV_ADMIN_EMAIL" ] || die "an admin email is required (set XDEV_ADMIN_EMAIL)"
    ask_secret XDEV_ADMIN_PASSWORD "Admin password (min 8 chars)"
  fi
  yesno XDEV_INSTALL_SERVICE "Install a background service?" "Y"
}

# --- 5. download the binary --------------------------------------------------
download_binary() {
  local base url tmp sums want got
  if [ "$XDEV_VERSION" = latest ]; then
    base="https://github.com/${XDEV_REPO}/releases/latest/download"
  else
    base="https://github.com/${XDEV_REPO}/releases/download/${XDEV_VERSION}"
  fi
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' RETURN

  info "downloading ${ASSET} (${XDEV_VERSION})…"
  if ! curl -fsSL "${base}/${ASSET}" -o "${tmp}/${ASSET}"; then
    warn "no release asset at ${base}/${ASSET}"
    build_from_source "$tmp"; return $?
  fi
  if curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt"; then
    want="$(awk -v a="$ASSET" '$2==a || $2=="*"a {print $1}' "${tmp}/checksums.txt" | head -n1)"
    if [ -n "$want" ]; then
      got="$(sha256_of "${tmp}/${ASSET}")"
      [ "$want" = "$got" ] || die "checksum mismatch for ${ASSET} (want ${want}, got ${got})"
      ok "checksum verified"
    else
      warn "checksums.txt has no entry for ${ASSET} — skipping verification"
    fi
  else
    warn "no checksums.txt in release — skipping verification"
  fi
  as_root install -m 0755 "${tmp}/${ASSET}" "${BIN_PATH}"
  ok "installed ${BIN_PATH}"
}

build_from_source() {
  local tmp="$1"
  have go || die "no prebuilt asset and Go not installed — cannot build from source"
  local here; here="$(cd "$(dirname "$0")/.." 2>/dev/null && pwd || echo "")"
  [ -f "${here}/go.mod" ] || die "no release asset and no source tree to build from"
  info "building xdev from source (${here})…"
  ( cd "$here" && CGO_ENABLED=0 go build -o "${tmp}/xdev" ./cmd/xdev )
  as_root install -m 0755 "${tmp}/xdev" "${BIN_PATH}"
  ok "built + installed ${BIN_PATH}"
}

# --- 6. write config ---------------------------------------------------------
write_config() {
  as_root mkdir -p "$(dirname "$ENV_PATH")" "$DATA_DIR" "${DATA_DIR}/projects"
  # Never silently clobber an edited config on a re-run — keep a .bak.
  if as_root test -f "$ENV_PATH"; then
    as_root cp -p "$ENV_PATH" "${ENV_PATH}.bak"
    info "backed up existing config → ${ENV_PATH}.bak"
  fi
  local manage_caddy="$MANAGE_CADDY"
  local tmp; tmp="$(mktemp)"
  cat > "$tmp" <<EOF
# xdev configuration — generated by install.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)
# See deploy/xdev.env.example for the full documented reference.
# --- core ---
XDEV_DATA=${DATA_DIR}
XDEV_PROJECTS=${DATA_DIR}/projects
XDEV_ADDR=${XDEV_ADDR}
XDEV_SECURE=${XDEV_SECURE}
# --- engine ---
XDEV_ENGINE=${ENGINE}
# --- proxy & TLS ---
XDEV_CADDY=${manage_caddy}
XDEV_CADDY_ADMIN=127.0.0.1:2019
XDEV_HTTPS_PORT=${XDEV_HTTPS_PORT}
XDEV_HTTP_PORT=${XDEV_HTTP_PORT}
XDEV_ACME_EMAIL=${XDEV_ACME_EMAIL:-}
XDEV_LOCAL_CERT_LIFETIME=${XDEV_LOCAL_CERT_LIFETIME:-2160h}
# --- hosts ---
XDEV_HOSTS_FILE=${XDEV_HOSTS_FILE:-/etc/hosts}
XDEV_MANAGE_HOSTS=${XDEV_MANAGE_HOSTS}
EOF
  as_root install -m 0640 "$tmp" "$ENV_PATH"
  rm -f "$tmp"
  ok "wrote ${ENV_PATH}"
}

# --- 7. service setup --------------------------------------------------------
install_service_linux() {
  [ "$XDEV_INSTALL_SERVICE" = true ] || { info "skipping systemd service (run xdev yourself)"; return 0; }
  as_root mkdir -p /opt/xdev
  local src; src="$(cd "$(dirname "$0")" 2>/dev/null && pwd || echo "")/xdev.service"
  if [ -f "$src" ]; then
    as_root install -m 0644 "$src" /etc/systemd/system/xdev.service
  else
    # Piped install (no local file): write the unit inline.
    as_root tee /etc/systemd/system/xdev.service >/dev/null <<'UNIT'
[Unit]
Description=xdev control plane
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
User=root
WorkingDirectory=/opt/xdev
EnvironmentFile=/etc/xdev/xdev.env
ExecStart=/usr/local/bin/xdev
AmbientCapabilities=CAP_NET_BIND_SERVICE
Restart=on-failure
RestartSec=3
[Install]
WantedBy=multi-user.target
UNIT
  fi
  as_root systemctl daemon-reload
  as_root systemctl enable --now xdev
  ok "systemd unit installed + started"
}

install_service_macos() {
  [ "$XDEV_INSTALL_SERVICE" = true ] || { info "skipping LaunchDaemon (run xdev yourself)"; return 0; }
  local plist=/Library/LaunchDaemons/com.leyume.xdev.plist
  local src; src="$(cd "$(dirname "$0")" 2>/dev/null && pwd || echo "")/com.leyume.xdev.plist"
  as_root mkdir -p /usr/local/var/log
  if [ -f "$src" ]; then
    as_root install -m 0644 "$src" "$plist"
  else
    as_root tee "$plist" >/dev/null <<'PL'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.leyume.xdev</string>
  <key>ProgramArguments</key><array>
    <string>/bin/bash</string><string>-c</string>
    <string>set -a; [ -f /usr/local/etc/xdev/xdev.env ] &amp;&amp; . /usr/local/etc/xdev/xdev.env; exec /usr/local/bin/xdev</string>
  </array>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/usr/local/var/log/xdev.log</string>
  <key>StandardErrorPath</key><string>/usr/local/var/log/xdev.log</string>
</dict></plist>
PL
  fi
  as_root launchctl bootout system "$plist" 2>/dev/null || true
  as_root launchctl bootstrap system "$plist" || as_root launchctl load "$plist" || true
  ok "LaunchDaemon installed ($plist)"
}

# --- 8. admin + finishing ----------------------------------------------------
create_admin() {
  if [ -z "${XDEV_ADMIN_EMAIL:-}" ]; then
    info "keeping existing admin account (none requested)"
    return 0
  fi
  info "ensuring admin ${XDEV_ADMIN_EMAIL}…"
  # sudo scrubs the environment, so pass the needed vars explicitly via `env`.
  # create-admin is idempotent: it no-ops (without needing a password) if an
  # admin already exists.
  as_root env XDEV_ADMIN_PASSWORD="${XDEV_ADMIN_PASSWORD:-}" XDEV_DATA="$DATA_DIR" \
    "$BIN_PATH" create-admin "$XDEV_ADMIN_EMAIL" && ok "admin ready"
  if [ -n "${XDEV_BASE_DOMAIN:-}" ]; then
    info "default base domain: ${XDEV_BASE_DOMAIN} (set it in the UI when creating projects)"
  fi
}

maybe_trust_ca() {
  [ "$OS" = darwin ] || return 0
  [ "$XDEV_MODE" = local ] || return 0
  [ "$MANAGE_CADDY" = true ] || return 0
  yesno XDEV_TRUST_CA "Trust xdev's local CA so https://*.localhost is green? (sudo caddy trust)" "Y"
  if [ "$XDEV_TRUST_CA" = true ] && have caddy; then
    sudo caddy trust 2>/dev/null && ok "local CA trusted" || warn "caddy trust failed (run it later: sudo caddy trust)"
  fi
}

print_next_steps() {
  echo
  info "running xdev doctor…"
  as_root env XDEV_DATA="$DATA_DIR" "$BIN_PATH" doctor \
    -caddy="$MANAGE_CADDY" -https-port "$XDEV_HTTPS_PORT" -http-port "$XDEV_HTTP_PORT" \
    -manage-hosts="$XDEV_MANAGE_HOSTS" || warn "doctor reported issues — review above"
  echo
  ok "${C_BOLD}xdev installed${C_RESET}"
  if [ "$XDEV_MODE" = prod ]; then
    cat <<EOF
Next steps:
  • Point DNS:  ${XDEV_BASE_DOMAIN}  A/AAAA  <this server's public IP>
  • Admin UI is private on ${XDEV_ADDR} — reach it via an SSH tunnel:
        ssh -L 7331:${XDEV_ADDR} ${USER:-you}@<server>   # then open http://127.0.0.1:7331
EOF
  else
    local b="${XDEV_BASE_DOMAIN:-<project>.localhost}"
    cat <<EOF
Next steps:
  • Open the admin UI:  http://${XDEV_ADDR}
  • Your apps will be served at  https://<app>.${b}
EOF
  fi
}

# --- main --------------------------------------------------------------------
main() {
  printf "%sxdev installer%s\n" "$C_BOLD" "$C_RESET"
  detect_platform
  ensure_privilege "$@"
  configure

  # Resolve install paths now that we know OS + privilege.
  BIN_PATH=/usr/local/bin/xdev
  if [ "$OS" = linux ]; then
    DATA_DIR="${XDEV_DATA:-/var/lib/xdev}"
    ENV_PATH=/etc/xdev/xdev.env
  else
    if [ "$(id -u)" -eq 0 ] || [ "$XDEV_INSTALL_SERVICE" = true ]; then
      DATA_DIR="${XDEV_DATA:-/var/lib/xdev}"
      ENV_PATH=/usr/local/etc/xdev/xdev.env
    else
      DATA_DIR="${XDEV_DATA:-$HOME/Library/Application Support/xdev/data}"
      ENV_PATH="$HOME/Library/Application Support/xdev/xdev.env"
    fi
  fi

  install_engine
  install_caddy
  download_binary
  write_config
  if [ "$OS" = linux ]; then install_service_linux; else install_service_macos; fi
  create_admin
  maybe_trust_ca
  print_next_steps
}

main "$@"
