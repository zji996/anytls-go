#!/usr/bin/env bash
set -euo pipefail

service_name="anytls-server"
config_dir="/etc/anytls"
env_file="$config_dir/server.env"
bin_path="/usr/local/bin/anytls-server"
systemd_unit="/etc/systemd/system/${service_name}.service"
default_branch="zji-dev"

usage() {
  cat <<'USAGE'
Usage:
  install-anytls-server.sh
  install-anytls-server.sh ACTION [options]

Actions:
  install            Install or reinstall anytls-server.
  update             Update source tree, rebuild, and restart service.
  status             Show service status and current client URI.
  doctor             Check local deployment prerequisites and service health.
  restart            Restart service.
  uninstall          Stop service and remove installed files.
  menu               Show interactive menu. Default when no action is given.

Options for install/update:
  -p, --password PASSWORD        AnyTLS password.
  -l, --listen ADDR             Listen address. Default: 0.0.0.0:8443
  -s, --server-name HOST        Host/IP used in generated client URI. Default: public IP.
      --fallback ADDR           Fallback address for invalid connections. Default: 127.0.0.1:80
      --branch BRANCH           Expected source branch. Default: zji-dev
      --binary FILE             Existing anytls-server binary to install instead of building.
      --padding-scheme FILE     Optional PaddingScheme file.
      --no-firewall             Do not try to open the listen port in local firewall.
      --non-interactive         Do not prompt; generate a password if missing.
  -h, --help                    Show this help.
USAGE
}

action="menu"
listen_addr=""
password=""
server_name=""
fallback_addr="127.0.0.1:80"
padding_scheme=""
binary_path=""
expected_branch="$default_branch"
non_interactive=0
fallback_arg_set=0
auto_firewall=1

if [[ $# -gt 0 ]]; then
  case "$1" in
    install|update|status|doctor|restart|uninstall|menu)
      action="$1"
      shift
      ;;
  esac
fi

while [[ $# -gt 0 ]]; do
  case "$1" in
    -p|--password)
      password="${2:-}"
      shift 2
      ;;
    -l|--listen)
      listen_addr="${2:-}"
      shift 2
      ;;
    -s|--server-name)
      server_name="${2:-}"
      shift 2
      ;;
    --padding-scheme)
      padding_scheme="${2:-}"
      shift 2
      ;;
    --fallback)
      fallback_addr="${2:-}"
      fallback_arg_set=1
      shift 2
      ;;
    --binary)
      binary_path="${2:-}"
      shift 2
      ;;
    --branch)
      expected_branch="${2:-}"
      shift 2
      ;;
    --non-interactive)
      non_interactive=1
      shift
      ;;
    --no-firewall)
      auto_firewall=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "please run as root, for example: sudo $0" >&2
    exit 1
  fi
}

require_systemd() {
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemd is required: systemctl not found" >&2
    exit 1
  fi
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"

env_quote() {
  printf "%s" "$1" | sed "s/'/'\\\\''/g; s/^/'/; s/$/'/"
}

read_env_value() {
  local key="$1"
  if [[ -f "$env_file" ]]; then
    sed -n "s/^${key}=//p" "$env_file" | tail -n 1 | sed "s/^'//; s/'$//; s/'\\\\''/'/g"
  fi
}

env_key_exists() {
  local key="$1"
  [[ -f "$env_file" ]] && grep -q "^${key}=" "$env_file"
}

detect_public_ip() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsS --max-time 5 https://api.ipify.org || true
  fi
}

prompt_default() {
  local prompt="$1"
  local default_value="$2"
  local value=""
  read -r -p "$prompt [$default_value]: " value </dev/tty
  printf "%s" "${value:-$default_value}"
}

prompt_secret_default() {
  local prompt="$1"
  local default_value="$2"
  local value=""
  if [[ -n "$default_value" ]]; then
    read -r -s -p "$prompt [keep existing]: " value </dev/tty
    echo >/dev/tty
    printf "%s" "${value:-$default_value}"
  else
    while [[ -z "$value" ]]; do
      read -r -s -p "$prompt: " value </dev/tty
      echo >/dev/tty
    done
    printf "%s" "$value"
  fi
}

generate_password() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 32 | tr '+/' '-_' | tr -d '='
  elif [[ -r /dev/urandom ]]; then
    LC_ALL=C tr -dc 'A-Za-z0-9_-' </dev/urandom | head -c 43
  else
    date +%s%N | sha256sum | awk '{print $1}'
  fi
}

prompt_password() {
  local default_value="$1"
  local value=""
  if [[ -n "$default_value" ]]; then
    read -r -s -p "Password [keep existing]: " value </dev/tty
    echo >/dev/tty
    printf "%s" "${value:-$default_value}"
    return
  fi

  default_value="$(generate_password)"
  echo "Generated password: $default_value" >/dev/tty
  read -r -s -p "Password [press Enter to use generated]: " value </dev/tty
  echo >/dev/tty
  printf "%s" "${value:-$default_value}"
}

prompt_yes_no() {
  local prompt="$1"
  local default_value="$2"
  local value=""
  read -r -p "$prompt [$default_value]: " value </dev/tty
  value="${value:-$default_value}"
  [[ "$value" == "y" || "$value" == "Y" || "$value" == "yes" || "$value" == "YES" ]]
}

load_existing_defaults() {
  local existing_listen existing_password existing_host existing_fallback
  existing_listen="$(read_env_value ANYTLS_LISTEN || true)"
  existing_password="$(read_env_value ANYTLS_PASSWORD || true)"
  existing_host="$(read_env_value ANYTLS_SERVER_NAME || true)"
  existing_fallback="$(read_env_value ANYTLS_FALLBACK || true)"
  listen_addr="${listen_addr:-${existing_listen:-0.0.0.0:8443}}"
  if [[ -z "$password" ]]; then
    password="$existing_password"
  fi
  if [[ -z "$server_name" ]]; then
    server_name="$existing_host"
  fi
  if [[ "$fallback_arg_set" -eq 0 ]] && env_key_exists ANYTLS_FALLBACK; then
    fallback_addr="$existing_fallback"
  fi
}

collect_install_inputs() {
  load_existing_defaults
  if [[ "$non_interactive" -eq 1 ]]; then
    if [[ -z "$password" ]]; then
      password="$(generate_password)"
    fi
    if [[ -z "$server_name" ]]; then
      server_name="$(detect_public_ip)"
    fi
    return
  fi

  echo
  echo "AnyTLS server setup"
  echo "Press Enter to accept defaults."
  echo

  password="$(prompt_password "$password")"
  listen_addr="$(prompt_default "Listen address" "${listen_addr:-0.0.0.0:8443}")"
  if [[ -z "$server_name" ]]; then
    server_name="$(detect_public_ip)"
  fi
  server_name="$(prompt_default "Server IP/domain for client URI" "${server_name:-YOUR_SERVER_IP}")"
  fallback_addr="$(prompt_default "Fallback address for invalid connections" "${fallback_addr:-127.0.0.1:80}")"

  if [[ -z "$padding_scheme" ]] && prompt_yes_no "Use custom PaddingScheme file?" "n"; then
    padding_scheme="$(prompt_default "PaddingScheme file path" "")"
  fi
}

install_padding_scheme() {
  padding_scheme_target=""
  if [[ -n "$padding_scheme" ]]; then
    if [[ ! -f "$padding_scheme" ]]; then
      echo "padding scheme file not found: $padding_scheme" >&2
      exit 1
    fi
    install -m 0644 "$padding_scheme" "$config_dir/padding.txt"
    padding_scheme_target="$config_dir/padding.txt"
  elif [[ -f "$config_dir/padding.txt" ]]; then
    padding_scheme_target="$config_dir/padding.txt"
  fi
}

build_or_install_binary() {
  if [[ -n "$binary_path" ]]; then
    if [[ ! -f "$binary_path" ]]; then
      echo "binary not found: $binary_path" >&2
      exit 1
    fi
    install -m 0755 "$binary_path" "$bin_path"
    return
  fi

  if ! command -v go >/dev/null 2>&1; then
    echo "Go is required to build from source. Run bootstrap script first, or use --binary FILE." >&2
    exit 1
  fi

  current_branch="$(cd "$repo_root" && git rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
  if [[ -n "$expected_branch" && "$current_branch" != "$expected_branch" ]]; then
    echo "warning: current branch is '${current_branch:-unknown}', expected '$expected_branch'" >&2
  fi

  echo "downloading Go modules..."
  (cd "$repo_root" && go mod download)

  echo "building anytls-server from $repo_root..."
  (cd "$repo_root" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$bin_path" ./cmd/server)
}

write_service_files() {
  install -d -m 0755 "$config_dir"
  install_padding_scheme

  cat >"$env_file" <<EOF
ANYTLS_LISTEN=$(env_quote "$listen_addr")
ANYTLS_PASSWORD=$(env_quote "$password")
ANYTLS_SERVER_NAME=$(env_quote "$server_name")
ANYTLS_FALLBACK=$(env_quote "$fallback_addr")
ANYTLS_PADDING_SCHEME=$(env_quote "$padding_scheme_target")
LOG_LEVEL=info
EOF
  chmod 0600 "$env_file"

  cat >"$systemd_unit" <<'EOF'
[Unit]
Description=AnyTLS Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/anytls/server.env
ExecStart=/bin/sh -c 'set -- -l "$ANYTLS_LISTEN" -p "$ANYTLS_PASSWORD" -fallback "$ANYTLS_FALLBACK"; if [ -n "$ANYTLS_PADDING_SCHEME" ]; then set -- "$@" -padding-scheme "$ANYTLS_PADDING_SCHEME"; fi; exec /usr/local/bin/anytls-server "$@"'
Restart=on-failure
RestartSec=3
LimitNOFILE=1048576
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true

[Install]
WantedBy=multi-user.target
EOF
}

uri_encode() {
  local raw="$1"
  python3 - "$raw" <<'PY' 2>/dev/null || printf '%s' "$raw"
import sys
from urllib.parse import quote
print(quote(sys.argv[1], safe=""))
PY
}

client_uri() {
  local host port encoded_password
  host="${server_name:-$(read_env_value ANYTLS_SERVER_NAME || true)}"
  if [[ -z "$host" ]]; then
    host="$(detect_public_ip)"
  fi
  if [[ -z "$host" ]]; then
    host="YOUR_SERVER_IP"
  fi
  port="${listen_addr##*:}"
  if [[ "$listen_addr" == *"]:"* ]]; then
    port="${listen_addr##*:}"
  fi
  encoded_password="$(uri_encode "$password")"
  echo "anytls://$encoded_password@$host:$port/?insecure=1"
}

listen_port() {
  local addr="$1"
  if [[ "$addr" == *"]:"* ]]; then
    printf "%s" "${addr##*:}"
  else
    printf "%s" "${addr##*:}"
  fi
}

open_firewall_port() {
  local port="$1"
  if [[ -z "$port" ]]; then
    echo "firewall: listen port is empty, skip"
    return
  fi
  if [[ "$auto_firewall" -ne 1 ]]; then
    echo "firewall: skipped by --no-firewall"
    return
  fi

  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    echo "firewall: opening tcp/$port with ufw"
    ufw allow "$port/tcp"
    return
  fi

  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    echo "firewall: opening tcp/$port with firewalld"
    firewall-cmd --permanent --add-port="$port/tcp"
    firewall-cmd --reload
    return
  fi

  if command -v iptables >/dev/null 2>&1; then
    if iptables -C INPUT -p tcp --dport "$port" -j ACCEPT >/dev/null 2>&1; then
      echo "firewall: iptables already allows tcp/$port"
    else
      echo "firewall: opening tcp/$port with iptables runtime rule"
      iptables -I INPUT -p tcp --dport "$port" -j ACCEPT
      echo "firewall: iptables runtime rules may not persist after reboot"
    fi
    return
  fi

  echo "firewall: no supported local firewall tool detected; remember to open tcp/$port in provider firewall/security group"
}

print_summary() {
  local port
  port="$(listen_port "$listen_addr")"
  echo
  echo "anytls-server is installed and started."
  echo "service: sudo systemctl status $service_name"
  echo "logs:    sudo journalctl -u $service_name -f"
  echo "port:    $port/tcp must be open in your cloud firewall/security group."
  if [[ -n "$fallback_addr" ]]; then
    echo "fallback: invalid AnyTLS traffic is forwarded to $fallback_addr."
  fi
  echo
  echo "Client URI:"
  client_uri
}

check_command() {
  local name="$1"
  if command -v "$name" >/dev/null 2>&1; then
    echo "  ok: $name"
  else
    echo "  warn: $name not found"
  fi
}

check_listen_port() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    if ss -ltn "( sport = :$port )" 2>/dev/null | awk 'NR > 1 { found=1 } END { exit found ? 0 : 1 }'; then
      echo "  ok: tcp/$port is listening"
    else
      echo "  warn: tcp/$port is not listening locally"
    fi
  else
    echo "  skip: ss not found, cannot check listening port"
  fi
}

check_firewall_port() {
  local port="$1"
  if [[ -z "$port" ]]; then
    echo "  skip: listen port is empty"
    return
  fi

  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    if ufw status numbered 2>/dev/null | grep -Eq "(^|[^0-9])${port}/tcp[[:space:]]+ALLOW"; then
      echo "  ok: ufw allows tcp/$port"
    else
      echo "  warn: ufw is active but tcp/$port is not explicitly allowed"
    fi
    return
  fi

  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    if firewall-cmd --query-port="$port/tcp" >/dev/null 2>&1; then
      echo "  ok: firewalld allows tcp/$port"
    else
      echo "  warn: firewalld is active but tcp/$port is not allowed"
    fi
    return
  fi

  if command -v iptables >/dev/null 2>&1; then
    if iptables -C INPUT -p tcp --dport "$port" -j ACCEPT >/dev/null 2>&1; then
      echo "  ok: iptables has runtime ACCEPT rule for tcp/$port"
    else
      echo "  info: no explicit iptables ACCEPT rule found for tcp/$port"
    fi
    return
  fi

  echo "  info: no supported local firewall tool detected"
}

check_fallback() {
  if [[ -z "$fallback_addr" ]]; then
    echo "  ok: fallback disabled"
    return
  fi
  local fallback_host fallback_port
  fallback_port="${fallback_addr##*:}"
  fallback_host="${fallback_addr%:*}"
  if [[ "$fallback_addr" == *"]:"* ]]; then
    fallback_port="${fallback_addr##*:}"
    fallback_host="${fallback_addr%:*}"
    fallback_host="${fallback_host#[}"
    fallback_host="${fallback_host%]}"
  fi
  if command -v ss >/dev/null 2>&1 && [[ "$fallback_host" == "127.0.0.1" || "$fallback_host" == "localhost" || "$fallback_host" == "::1" ]]; then
    if ss -ltn "( sport = :$fallback_port )" 2>/dev/null | awk 'NR > 1 { found=1 } END { exit found ? 0 : 1 }'; then
      echo "  ok: fallback target $fallback_addr is listening"
    else
      echo "  warn: fallback target $fallback_addr is not listening; active probes may see a closed backend"
    fi
  else
    echo "  info: fallback target $fallback_addr not checked locally"
  fi
}

run_doctor() {
  require_systemd
  load_existing_defaults
  local current_listen current_port
  current_listen="${listen_addr:-$(read_env_value ANYTLS_LISTEN || true)}"
  current_port="${current_listen##*:}"

  echo "AnyTLS deployment doctor"
  echo
  echo "Commands:"
  check_command systemctl
  check_command git
  check_command go
  check_command curl
  check_command ss
  echo
  echo "Service:"
  if systemctl is-enabled "$service_name" >/dev/null 2>&1; then
    echo "  ok: $service_name is enabled"
  else
    echo "  warn: $service_name is not enabled"
  fi
  if systemctl is-active "$service_name" >/dev/null 2>&1; then
    echo "  ok: $service_name is active"
  else
    echo "  warn: $service_name is not active"
  fi
  if [[ -x "$bin_path" ]]; then
    echo "  ok: binary exists at $bin_path"
  else
    echo "  warn: binary missing at $bin_path"
  fi
  if [[ -f "$env_file" ]]; then
    echo "  ok: config exists at $env_file"
  else
    echo "  warn: config missing at $env_file"
  fi
  if [[ -n "$current_port" ]]; then
    check_listen_port "$current_port"
  fi
  echo
  echo "Firewall:"
  if [[ -n "$current_port" ]]; then
    check_firewall_port "$current_port"
  fi
  echo
  echo "Fallback:"
  check_fallback
  echo
  echo "Client URI:"
  client_uri
  echo
  echo "Reminder: also open tcp/${current_port:-8443} in your cloud security group or provider firewall."
}

install_or_update() {
  require_root
  require_systemd
  collect_install_inputs
  install -d -m 0755 "$config_dir"
  build_or_install_binary
  write_service_files
  open_firewall_port "$(listen_port "$listen_addr")"
  systemctl daemon-reload
  systemctl enable --now "$service_name"
  systemctl restart "$service_name"
  print_summary
  echo
  run_doctor
}

update_source_tree() {
  if [[ -d "$repo_root/.git" ]]; then
    echo "updating source tree on branch $expected_branch..."
    git -C "$repo_root" fetch origin "$expected_branch"
    git -C "$repo_root" checkout "$expected_branch"
    git -C "$repo_root" pull --ff-only origin "$expected_branch"
  fi
}

do_update() {
  require_root
  require_systemd
  load_existing_defaults
  if [[ -z "$password" ]]; then
    echo "no existing installation found; running install instead"
    install_or_update
    return
  fi
  update_source_tree
  install_or_update
}

show_status() {
  require_systemd
  load_existing_defaults
  echo "Service:"
  systemctl --no-pager --full status "$service_name" || true
  echo
  if [[ -f "$env_file" ]]; then
    echo "Config:"
    echo "  listen: ${listen_addr:-$(read_env_value ANYTLS_LISTEN || true)}"
    echo "  server: ${server_name:-$(read_env_value ANYTLS_SERVER_NAME || true)}"
    echo "  fallback: ${fallback_addr:-$(read_env_value ANYTLS_FALLBACK || true)}"
    echo "  env:    $env_file"
    echo
    echo "Client URI:"
    client_uri
  else
    echo "No config found at $env_file"
  fi
}

do_restart() {
  require_root
  require_systemd
  systemctl restart "$service_name"
  show_status
}

do_uninstall() {
  require_root
  require_systemd
  if [[ "$non_interactive" -ne 1 ]]; then
    if ! prompt_yes_no "Uninstall anytls-server and remove config?" "n"; then
      echo "cancelled"
      return
    fi
  fi
  systemctl disable --now "$service_name" 2>/dev/null || true
  rm -f "$systemd_unit" "$bin_path"
  rm -rf "$config_dir"
  systemctl daemon-reload
  echo "anytls-server uninstalled."
}

show_menu() {
  require_root
  require_systemd
  while true; do
    echo
    echo "AnyTLS Server Manager"
    echo "1) Install / Reinstall"
    echo "2) Update zji-dev and restart"
    echo "3) Status and client URI"
    echo "4) Doctor / self-check"
    echo "5) Restart"
    echo "6) Uninstall"
    echo "0) Exit"
    read -r -p "Select [1]: " choice </dev/tty
    choice="${choice:-1}"
    case "$choice" in
      1) install_or_update ;;
      2) do_update ;;
      3) show_status ;;
      4) run_doctor ;;
      5) do_restart ;;
      6) do_uninstall ;;
      0) exit 0 ;;
      *) echo "invalid choice" ;;
    esac
  done
}

case "$action" in
  install) install_or_update ;;
  update) do_update ;;
  status) show_status ;;
  doctor) run_doctor ;;
  restart) do_restart ;;
  uninstall) do_uninstall ;;
  menu) show_menu ;;
  *) usage >&2; exit 2 ;;
esac
