#!/usr/bin/env bash
set -euo pipefail

service_name="${ANYTLS_SERVICE_NAME:-anytls-server}"
service_user="${ANYTLS_SERVICE_USER:-anytls}"
service_group="${ANYTLS_SERVICE_GROUP:-$service_user}"
config_dir="${ANYTLS_CONFIG_DIR:-/etc/anytls}"
env_file="${ANYTLS_ENV_FILE:-$config_dir/server.env}"
password_file="${ANYTLS_PASSWORD_FILE:-$config_dir/server.password}"
firewall_state_file="${ANYTLS_FIREWALL_STATE_FILE:-$config_dir/firewall.state}"
bin_path="${ANYTLS_BIN_PATH:-/usr/local/bin/anytls-server}"
systemd_unit="${ANYTLS_SYSTEMD_UNIT:-/etc/systemd/system/${service_name}.service}"
deployment_lock_file="${ANYTLS_DEPLOYMENT_LOCK_FILE:-/run/lock/${service_name}.install.lock}"
default_branch="zji-dev"

transaction_dir=""
transaction_active=0
service_was_active=0
service_was_enabled=0
service_user_created=0
config_dir_existed=0
config_dir_metadata=""
deployment_lock_fd=""

usage() {
  cat <<'USAGE'
Usage:
  install-anytls-server.sh
  install-anytls-server.sh ACTION [options]

Actions:
  install            Install or reinstall anytls-server.
  update             Safely update source, rebuild, and restart service.
  status             Show service status and current client URI.
  doctor             Check deployment prerequisites and service health.
  restart            Restart service.
  uninstall          Stop service and remove managed files and firewall rules.
  menu               Show interactive menu. Default when no action is given.

Options for install/update:
  -p, --password PASSWORD        AnyTLS password.
  -l, --listen ADDR             Listen address. Default: 0.0.0.0:8443
  -s, --server-name HOST        Host/IP used in generated client URI. Default: public IP.
      --fallback ADDR           Fallback address. Empty disables it. Default: 127.0.0.1:80
      --branch BRANCH           Expected source branch. Default: zji-dev
      --binary FILE             Existing server binary; Go still builds the health probe.
      --padding-scheme FILE     Optional PaddingScheme file.
      --no-firewall             Do not modify local firewall rules.
      --non-interactive         Do not prompt; generate a password if missing.
  -h, --help                    Show this help.
USAGE
}

action="menu"
listen_addr=""
password=""
password_arg_set=0
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

require_option_value() {
  if [[ $# -lt 2 || -z "$2" ]]; then
    echo "option $1 requires a value" >&2
    exit 2
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -p|--password)
      require_option_value "$@"
      password="$2"
      password_arg_set=1
      shift 2
      ;;
    -l|--listen)
      require_option_value "$@"
      listen_addr="$2"
      shift 2
      ;;
    -s|--server-name)
      require_option_value "$@"
      server_name="$2"
      shift 2
      ;;
    --padding-scheme)
      require_option_value "$@"
      padding_scheme="$2"
      shift 2
      ;;
    --fallback)
      if [[ $# -lt 2 ]]; then
        echo "option $1 requires a value (use an empty string to disable fallback)" >&2
        exit 2
      fi
      fallback_addr="$2"
      fallback_arg_set=1
      shift 2
      ;;
    --binary)
      require_option_value "$@"
      binary_path="$2"
      shift 2
      ;;
    --branch)
      require_option_value "$@"
      expected_branch="$2"
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

with_deployment_lock() {
  if [[ "${ANYTLS_DEPLOYMENT_LOCK_HELD:-0}" == "1" ]]; then
    "$@"
    return
  fi
  if ! command -v flock >/dev/null 2>&1; then
    echo "flock is required for safe deployment; install util-linux" >&2
    return 1
  fi
  install -d -m 0755 "$(dirname "$deployment_lock_file")" || return 1
  if ! exec {deployment_lock_fd}>"$deployment_lock_file"; then
    deployment_lock_fd=""
    return 1
  fi
  if ! flock -n "$deployment_lock_fd"; then
    echo "another AnyTLS deployment is already running (lock: $deployment_lock_file)" >&2
    exec {deployment_lock_fd}>&-
    deployment_lock_fd=""
    return 75
  fi
  local status=0
  ANYTLS_DEPLOYMENT_LOCK_HELD=1 "$@" || status=$?
  flock -u "$deployment_lock_fd" || true
  exec {deployment_lock_fd}>&-
  deployment_lock_fd=""
  return "$status"
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"

env_quote() {
  printf "%s" "$1" | sed "s/'/'\\\\''/g; s/^/'/; s/$/'/"
}

read_env_value() {
  local key="$1"
  if [[ -f "$env_file" ]]; then
    (
      set -a
      # The file is root-owned and generated by this installer.
      # shellcheck disable=SC1090
      source "$env_file"
      printf '%s' "${!key-}"
    )
  fi
}

env_key_exists() {
  local key="$1"
  [[ -f "$env_file" ]] && grep -q "^${key}=" "$env_file" 2>/dev/null
}

read_existing_password() {
  if [[ -f "$password_file" ]]; then
    IFS= read -r password_from_file <"$password_file" || true
    printf '%s' "${password_from_file:-}"
  else
    read_env_value ANYTLS_PASSWORD || true
  fi
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

prompt_password() {
  local default_value="$1"
  local value=""
  if [[ -n "$default_value" ]]; then
    read -r -s -p "Password [keep existing]: " value </dev/tty
    echo >/dev/tty
    printf "%s" "${value:-$default_value}"
    return
  fi

  default_value="$(generate_password)" || return 1
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
  [[ "$value" =~ ^([yY]|[yY][eE][sS])$ ]]
}

generate_password() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'
  elif [[ -r /dev/urandom ]] && command -v base64 >/dev/null 2>&1; then
    head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n'
  elif [[ -r /dev/urandom ]] && command -v od >/dev/null 2>&1; then
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
  else
    echo "cannot securely generate a password: openssl, base64, or od is required" >&2
    return 1
  fi
}

load_existing_defaults() {
  local existing_listen existing_password existing_host existing_fallback
  if [[ -f "$env_file" ]]; then
    if ! (
      set -a
      # shellcheck disable=SC1090
      source "$env_file"
    ); then
      echo "existing environment file is invalid: $env_file" >&2
      return 1
    fi
  fi
  existing_listen="$(read_env_value ANYTLS_LISTEN || true)"
  existing_password="$(read_existing_password || true)"
  existing_host="$(read_env_value ANYTLS_SERVER_NAME || true)"
  existing_fallback="$(read_env_value ANYTLS_FALLBACK || true)"
  listen_addr="${listen_addr:-${existing_listen:-0.0.0.0:8443}}"
  password="${password:-$existing_password}"
  server_name="${server_name:-$existing_host}"
  if [[ "$fallback_arg_set" -eq 0 ]] && env_key_exists ANYTLS_FALLBACK; then
    fallback_addr="$existing_fallback"
  fi
}

collect_install_inputs() {
  load_existing_defaults || return 1
  if [[ "$non_interactive" -eq 1 ]]; then
    if [[ -z "$password" ]]; then
      password="$(generate_password)" || return 1
    fi
    server_name="${server_name:-$(detect_public_ip)}"
    return
  fi

  echo
  echo "AnyTLS server setup"
  echo "Press Enter to accept defaults."
  echo
  password="$(prompt_password "$password")" || return 1
  listen_addr="$(prompt_default "Listen address" "${listen_addr:-0.0.0.0:8443}")" || return 1
  server_name="${server_name:-$(detect_public_ip)}"
  server_name="$(prompt_default "Server IP/domain for client URI" "${server_name:-YOUR_SERVER_IP}")" || return 1
  fallback_addr="$(prompt_default "Fallback address (type 'none' to disable)" "${fallback_addr:-127.0.0.1:80}")" || return 1
  [[ "$fallback_addr" == "none" || "$fallback_addr" == "off" ]] && fallback_addr=""
  if [[ -z "$padding_scheme" ]] && prompt_yes_no "Use custom PaddingScheme file?" "n"; then
    padding_scheme="$(prompt_default "PaddingScheme file path" "")" || return 1
  fi
}

split_endpoint() {
  local endpoint="$1"
  endpoint_host=""
  endpoint_port=""
  if [[ "$endpoint" =~ ^\[([^]]+)\]:([0-9]+)$ ]]; then
    endpoint_host="${BASH_REMATCH[1]}"
    endpoint_port="${BASH_REMATCH[2]}"
  elif [[ "$endpoint" =~ ^([^:]*):([0-9]+)$ ]]; then
    endpoint_host="${BASH_REMATCH[1]}"
    endpoint_port="${BASH_REMATCH[2]}"
  else
    return 1
  fi
  if [[ -n "$endpoint_host" && ! "$endpoint_host" =~ ^[A-Za-z0-9._:%-]+$ ]]; then
    return 1
  fi
  (( endpoint_port >= 1 && endpoint_port <= 65535 ))
}

validate_inputs() {
  if [[ -z "$password" || "$password" == *$'\n'* || "$password" == *$'\r'* ]]; then
    echo "password must be non-empty and must not contain newlines" >&2
    return 1
  fi
  if ! split_endpoint "$listen_addr"; then
    echo "invalid listen address '$listen_addr'; use HOST:PORT or [IPv6]:PORT" >&2
    return 1
  fi
  local normalized_server_name="${server_name#[}"
  normalized_server_name="${normalized_server_name%]}"
  if [[ -z "$normalized_server_name" || "$normalized_server_name" == "YOUR_SERVER_IP" || ! "$normalized_server_name" =~ ^[A-Za-z0-9._:%-]+$ ]]; then
    echo "invalid server name '$server_name'; pass a public IP or DNS name with --server-name" >&2
    return 1
  fi
  if [[ -n "$fallback_addr" ]] && ! split_endpoint "$fallback_addr"; then
    echo "invalid fallback address '$fallback_addr'; use HOST:PORT, [IPv6]:PORT, or an empty string" >&2
    return 1
  fi
  if [[ -n "$padding_scheme" && ! -r "$padding_scheme" ]]; then
    echo "padding scheme file not readable: $padding_scheme" >&2
    return 1
  fi
  if [[ -n "$binary_path" && ! -f "$binary_path" ]]; then
    echo "binary not found: $binary_path" >&2
    return 1
  fi
  if [[ -z "$expected_branch" || "$expected_branch" == -* ]]; then
    echo "invalid branch name: $expected_branch" >&2
    return 1
  fi
  if command -v git >/dev/null 2>&1 && ! git check-ref-format --branch "$expected_branch" >/dev/null 2>&1; then
    echo "invalid branch name: $expected_branch" >&2
    return 1
  fi
}

group_exists() {
  if command -v getent >/dev/null 2>&1; then
    getent group "$1" >/dev/null 2>&1
  else
    grep -q "^${1}:" /etc/group 2>/dev/null
  fi
}

ensure_service_user() {
  service_user_created=0
  if id "$service_user" >/dev/null 2>&1; then
    if ! group_exists "$service_group"; then
      echo "service group does not exist: $service_group" >&2
      return 1
    fi
    return
  fi
  echo "creating system user $service_user..."
  if command -v useradd >/dev/null 2>&1; then
    if group_exists "$service_group"; then
      useradd --system --gid "$service_group" --home-dir /nonexistent --shell /usr/sbin/nologin "$service_user" || return 1
    else
      useradd --system --user-group --home-dir /nonexistent --shell /usr/sbin/nologin "$service_user" || return 1
    fi
  elif command -v adduser >/dev/null 2>&1; then
    adduser -S -D -H -s /sbin/nologin "$service_user" || return 1
  else
    echo "cannot create service user: useradd/adduser not found" >&2
    return 1
  fi
  service_user_created=1
}

remove_created_service_user() {
  if [[ "${service_user_created:-0}" -eq 1 ]]; then
    userdel "$service_user" >/dev/null 2>&1 || deluser "$service_user" >/dev/null 2>&1 || true
    service_user_created=0
  fi
}

run_candidate_tests() {
  if [[ "${ANYTLS_SKIP_CANDIDATE_TESTS:-0}" == "1" ]]; then
    return
  fi
  echo "downloading Go modules and running candidate tests..."
  if ! (
    cd "$repo_root" &&
      go mod download &&
      go test ./... &&
      bash -n scripts/install-anytls-server.sh scripts/bootstrap-anytls-server.sh scripts/test-installation.sh &&
      ANYTLS_SKIP_CANDIDATE_TESTS=1 bash scripts/test-installation.sh
  ); then
    return 1
  fi
}

build_staged_binaries() {
  local stage_dir="$1"
  if ! command -v go >/dev/null 2>&1; then
    echo "Go is required to build the health probe. Run bootstrap first." >&2
    return 1
  fi

  if [[ -n "$binary_path" ]]; then
    install -m 0755 "$binary_path" "$stage_dir/anytls-server" || return 1
  fi

  local current_branch
  current_branch="$(git -C "$repo_root" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
  if [[ "${ANYTLS_UPDATE_STAGED:-0}" != "1" && -n "$expected_branch" && "$current_branch" != "$expected_branch" ]]; then
    echo "warning: current branch is '${current_branch:-unknown}', expected '$expected_branch'" >&2
  fi
  run_candidate_tests || return 1
  echo "building AnyTLS candidates from $repo_root..."
  if [[ -z "$binary_path" ]]; then
    (cd "$repo_root" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$stage_dir/anytls-server" ./cmd/server) || return 1
  fi
  (cd "$repo_root" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$stage_dir/anytls-client" ./cmd/client) || return 1
}

write_staged_files() {
  local stage_dir="$1"
  printf '%s' "$password" >"$stage_dir/server.password" || return 1
  chmod 0640 "$stage_dir/server.password" || return 1

  local staged_padding=""
  if [[ -n "$padding_scheme" ]]; then
    install -m 0640 "$padding_scheme" "$stage_dir/padding.txt" || return 1
    staged_padding="$config_dir/padding.txt"
  elif [[ -f "$config_dir/padding.txt" ]]; then
    install -m 0640 "$config_dir/padding.txt" "$stage_dir/padding.txt" || return 1
    staged_padding="$config_dir/padding.txt"
  fi

  if ! cat >"$stage_dir/server.env" <<EOF
ANYTLS_LISTEN=$(env_quote "$listen_addr")
ANYTLS_PASSWORD_FILE=$(env_quote "$password_file")
ANYTLS_SERVER_NAME=$(env_quote "$server_name")
ANYTLS_FALLBACK=$(env_quote "$fallback_addr")
ANYTLS_PADDING_SCHEME=$(env_quote "$staged_padding")
LOG_LEVEL=info
EOF
  then
    return 1
  fi
  chmod 0640 "$stage_dir/server.env" || return 1

  if ! cat >"$stage_dir/${service_name}.service" <<EOF
[Unit]
Description=AnyTLS Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$service_user
Group=$service_group
EnvironmentFile=$env_file
ExecStart=$bin_path
Restart=on-failure
RestartSec=3
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectControlGroups=true
ProtectKernelModules=true
ProtectKernelTunables=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF
  then
    return 1
  fi
  chmod 0644 "$stage_dir/${service_name}.service" || return 1
}

backup_path() {
  local path="$1"
  local name="$2"
  if [[ -e "$path" || -L "$path" ]]; then
    cp -a "$path" "$transaction_dir/backup/$name" || return 1
    : >"$transaction_dir/backup/$name.exists" || return 1
  fi
}

restore_path() {
  local path="$1"
  local name="$2"
  if [[ -f "$transaction_dir/backup/$name.exists" ]]; then
    rm -rf "$path" || return 1
    cp -a "$transaction_dir/backup/$name" "$path" || return 1
  else
    rm -rf "$path" || return 1
  fi
}

transaction_exit() {
  local status="$1"
  trap - EXIT INT TERM
  if [[ "$transaction_active" -eq 1 ]]; then
    if ! rollback_install; then
      echo "rollback failed; manual recovery is required from $transaction_dir/backup" >&2
      status=1
      transaction_dir=""
    fi
  fi
  if [[ -n "$transaction_dir" && -d "$transaction_dir" ]]; then
    rm -rf "$transaction_dir"
  fi
  exit "$status"
}

begin_install_transaction() {
  transaction_dir="$1"
  mkdir -p "$transaction_dir/backup" || return 1
  service_was_active=0
  service_was_enabled=0
  config_dir_existed=0
  config_dir_metadata=""
  systemctl is-active "$service_name" >/dev/null 2>&1 && service_was_active=1
  systemctl is-enabled "$service_name" >/dev/null 2>&1 && service_was_enabled=1
  if [[ -d "$config_dir" ]]; then
    config_dir_existed=1
    config_dir_metadata="$(stat -c '%u:%g %a' "$config_dir")" || return 1
  elif [[ -e "$config_dir" || -L "$config_dir" ]]; then
    echo "configuration path exists but is not a directory: $config_dir" >&2
    return 1
  fi

  backup_path "$bin_path" binary || return 1
  backup_path "$env_file" environment || return 1
  backup_path "$password_file" password || return 1
  backup_path "$config_dir/padding.txt" padding || return 1
  backup_path "$config_dir/user.managed" user_managed || return 1
  backup_path "$systemd_unit" unit || return 1
  transaction_active=1
  trap 'transaction_exit $?' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
}

commit_install_transaction() {
  transaction_active=0
  trap - EXIT INT TERM
}

rollback_install() {
  echo "installation failed; restoring previous files..." >&2
  transaction_active=0
  local rollback_failed=0
  restore_path "$bin_path" binary || rollback_failed=1
  restore_path "$env_file" environment || rollback_failed=1
  restore_path "$password_file" password || rollback_failed=1
  restore_path "$config_dir/padding.txt" padding || rollback_failed=1
  restore_path "$config_dir/user.managed" user_managed || rollback_failed=1
  restore_path "$systemd_unit" unit || rollback_failed=1
  if [[ "$config_dir_existed" -eq 1 ]]; then
    if [[ -n "$config_dir_metadata" ]]; then
      chown "${config_dir_metadata%% *}" "$config_dir" || rollback_failed=1
      chmod "${config_dir_metadata##* }" "$config_dir" || rollback_failed=1
    fi
  else
    rm -rf "$config_dir" || rollback_failed=1
  fi
  systemctl daemon-reload >/dev/null 2>&1 || rollback_failed=1
  if [[ "$service_was_enabled" -eq 1 ]]; then
    systemctl enable "$service_name" >/dev/null 2>&1 || rollback_failed=1
  else
    systemctl disable "$service_name" >/dev/null 2>&1 || true
  fi
  if [[ "$service_was_active" -eq 1 ]]; then
    if ! systemctl restart "$service_name" >/dev/null 2>&1 || ! wait_for_service_stable; then
      rollback_failed=1
    fi
  else
    systemctl stop "$service_name" >/dev/null 2>&1 || true
  fi
  if [[ "${service_user_created:-0}" -eq 1 ]]; then
    if ! userdel "$service_user" >/dev/null 2>&1 && ! deluser "$service_user" >/dev/null 2>&1; then
      rollback_failed=1
    fi
  fi
  if [[ "$rollback_failed" -ne 0 ]]; then
    echo "previous installation could not be fully restored" >&2
    return 1
  fi
  echo "previous installation restored" >&2
}

wait_for_service_stable() {
  local main_pid current_pid
  main_pid=""
  for _ in {1..20}; do
    if systemctl is-active "$service_name" >/dev/null 2>&1; then
      current_pid="$(systemctl show --property MainPID --value "$service_name" 2>/dev/null || true)"
      if [[ "$current_pid" =~ ^[1-9][0-9]*$ ]]; then
        if [[ -z "$main_pid" ]]; then
          main_pid="$current_pid"
        elif [[ "$current_pid" != "$main_pid" ]]; then
          main_pid="$current_pid"
        fi
        break
      fi
    fi
    sleep 0.25
  done

  if [[ -n "$main_pid" ]]; then
    for _ in {1..12}; do
      current_pid="$(systemctl show --property MainPID --value "$service_name" 2>/dev/null || true)"
      if ! systemctl is-active "$service_name" >/dev/null 2>&1 || [[ "$current_pid" != "$main_pid" ]]; then
        main_pid=""
        break
      fi
      sleep 0.25
    done
  fi
  if [[ -n "$main_pid" ]]; then
    return 0
  fi
  systemctl --no-pager --full status "$service_name" >&2 || true
  journalctl -u "$service_name" -n 30 --no-pager >&2 2>/dev/null || true
  return 1
}

atomic_install() {
  local source="$1"
  local target="$2"
  local owner="$3"
  local group="$4"
  local mode="$5"
  local temp
  temp="$(mktemp "${target}.new.XXXXXX")" || return 1
  if ! install -o "$owner" -g "$group" -m "$mode" "$source" "$temp"; then
    rm -f "$temp"
    return 1
  fi
  if ! mv -f "$temp" "$target"; then
    rm -f "$temp"
    return 1
  fi
}

apply_staged_install() {
  local stage_dir="$1"
  install -d -o root -g "$service_group" -m 0750 "$config_dir" || return 1
  atomic_install "$stage_dir/anytls-server" "$bin_path" root root 0755 || return 1
  atomic_install "$stage_dir/server.env" "$env_file" root "$service_group" 0640 || return 1
  atomic_install "$stage_dir/server.password" "$password_file" root "$service_group" 0640 || return 1
  if [[ -f "$stage_dir/padding.txt" ]]; then
    atomic_install "$stage_dir/padding.txt" "$config_dir/padding.txt" root "$service_group" 0640 || return 1
  else
    rm -f "$config_dir/padding.txt" || return 1
  fi
  atomic_install "$stage_dir/${service_name}.service" "$systemd_unit" root root 0644 || return 1
  if [[ "${service_user_created:-0}" -eq 1 ]]; then
    : >"$config_dir/user.managed" || return 1
    chown root:root "$config_dir/user.managed" || return 1
    chmod 0600 "$config_dir/user.managed" || return 1
  fi

  if ! systemctl daemon-reload || ! systemctl enable "$service_name" || ! systemctl restart "$service_name" || ! wait_for_service_stable; then
    return 1
  fi
}

uri_encode() {
  local raw="$1"
  local output="" char hex index
  local LC_ALL=C
  for ((index = 0; index < ${#raw}; index++)); do
    char="${raw:index:1}"
    case "$char" in
      [a-zA-Z0-9.~_-]) output+="$char" ;;
      *)
        printf -v hex '%02X' "'$char"
        output+="%$hex"
        ;;
    esac
  done
  printf '%s' "$output"
}

listen_port() {
  split_endpoint "$1" || return 1
  printf '%s' "$endpoint_port"
}

probe_server_address() {
  split_endpoint "$listen_addr" || return 1
  local host="$endpoint_host"
  case "$host" in
    ""|0.0.0.0|"*") host="127.0.0.1" ;;
    ::|"[::]") host="::1" ;;
  esac
  if [[ "$host" == *:* ]]; then
    printf '[%s]:%s' "$host" "$endpoint_port"
  else
    printf '%s:%s' "$host" "$endpoint_port"
  fi
}

run_anytls_probe() {
  local probe_binary="$1"
  local server_address
  server_address="$(probe_server_address)" || return 1
  echo "running end-to-end AnyTLS health probe against $server_address..."
  if [[ -n "${ANYTLS_HEALTH_PROBE_COMMAND:-}" ]]; then
    "$ANYTLS_HEALTH_PROBE_COMMAND" "$server_address" "$password_file"
    return
  fi
  LOG_LEVEL=warn "$probe_binary" \
    -probe \
    -s "$server_address" \
    -password-file "$password_file" \
    -insecure=true \
    -m 0
}

normalize_uri_host() {
  local host="$1"
  host="${host#[}"
  host="${host%]}"
  if [[ "$host" == *:* ]]; then
    host="${host//%/%25}"
    printf '[%s]' "$host"
  else
    printf '%s' "$host"
  fi
}

client_uri() {
  local host port encoded_password current_listen current_password
  host="${server_name:-$(read_env_value ANYTLS_SERVER_NAME || true)}"
  host="${host:-$(detect_public_ip)}"
  host="${host:-YOUR_SERVER_IP}"
  current_listen="${listen_addr:-$(read_env_value ANYTLS_LISTEN || true)}"
  current_listen="${current_listen:-0.0.0.0:8443}"
  port="$(listen_port "$current_listen")"
  current_password="${password:-$(read_existing_password || true)}"
  encoded_password="$(uri_encode "$current_password")"
  echo "anytls://$encoded_password@$(normalize_uri_host "$host"):$port/?insecure=1"
}

read_firewall_state() {
  managed_firewall_backend=""
  managed_firewall_port=""
  if [[ -f "$firewall_state_file" ]]; then
    IFS=' ' read -r managed_firewall_backend managed_firewall_port <"$firewall_state_file" || true
  fi
}

remove_firewall_rule() {
  local backend="$1"
  local port="$2"
  [[ -n "$backend" && -n "$port" ]] || return 0
  echo "firewall: removing managed $backend rule for tcp/$port"
  case "$backend" in
    ufw) ufw --force delete allow "$port/tcp" >/dev/null 2>&1 || true ;;
    firewalld)
      firewall-cmd --permanent --remove-port="$port/tcp" >/dev/null 2>&1 || true
      firewall-cmd --reload >/dev/null 2>&1 || true
      ;;
    iptables) iptables -D INPUT -p tcp --dport "$port" -j ACCEPT >/dev/null 2>&1 || true ;;
  esac
}

ensure_firewall_rule() {
  local port="$1"
  added_firewall_backend=""
  firewall_rule_ready=0
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    if ufw status 2>/dev/null | grep -Eq "(^|[^0-9])${port}/tcp[[:space:]]+ALLOW"; then
      echo "firewall: ufw already allows tcp/$port"
      firewall_rule_ready=1
    else
      echo "firewall: opening tcp/$port with ufw"
      if ufw allow "$port/tcp"; then
        added_firewall_backend="ufw"
        firewall_rule_ready=1
      else
        echo "firewall: failed to add ufw rule; configure it manually" >&2
      fi
    fi
    return
  fi
  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    if firewall-cmd --query-port="$port/tcp" >/dev/null 2>&1; then
      echo "firewall: firewalld already allows tcp/$port"
      firewall_rule_ready=1
    else
      echo "firewall: opening tcp/$port with firewalld"
      if firewall-cmd --permanent --add-port="$port/tcp" && firewall-cmd --reload; then
        added_firewall_backend="firewalld"
        firewall_rule_ready=1
      else
        echo "firewall: failed to add firewalld rule; configure it manually" >&2
      fi
    fi
    return
  fi
  if command -v iptables >/dev/null 2>&1; then
    if iptables -C INPUT -p tcp --dport "$port" -j ACCEPT >/dev/null 2>&1; then
      echo "firewall: iptables already allows tcp/$port"
      firewall_rule_ready=1
    else
      echo "firewall: opening tcp/$port with iptables runtime rule"
      if iptables -I INPUT -p tcp --dport "$port" -j ACCEPT; then
        echo "firewall: iptables runtime rules may not persist after reboot"
        added_firewall_backend="iptables"
        firewall_rule_ready=1
      else
        echo "firewall: failed to add iptables rule; configure it manually" >&2
      fi
    fi
    return
  fi
  echo "firewall: no supported tool detected; open tcp/$port in the provider firewall"
}

reconcile_firewall() {
  local new_port="$1"
  read_firewall_state
  if [[ "$auto_firewall" -ne 1 ]]; then
    echo "firewall: skipped by --no-firewall; existing managed rule left unchanged"
    return
  fi
  if [[ "$managed_firewall_port" == "$new_port" && -n "$managed_firewall_backend" ]]; then
    case "$managed_firewall_backend" in
      ufw) ufw status 2>/dev/null | grep -Eq "(^|[^0-9])${new_port}/tcp[[:space:]]+ALLOW" && { echo "firewall: managed ufw rule already covers tcp/$new_port"; return; } ;;
      firewalld) firewall-cmd --query-port="$new_port/tcp" >/dev/null 2>&1 && { echo "firewall: managed firewalld rule already covers tcp/$new_port"; return; } ;;
      iptables) iptables -C INPUT -p tcp --dport "$new_port" -j ACCEPT >/dev/null 2>&1 && { echo "firewall: managed iptables rule already covers tcp/$new_port"; return; } ;;
    esac
    echo "firewall: recorded rule is missing; recreating tcp/$new_port"
  fi
  ensure_firewall_rule "$new_port"
  if [[ "$firewall_rule_ready" -ne 1 ]]; then
    echo "firewall: tcp/$new_port was not opened automatically; previous managed rule was retained" >&2
    echo "firewall: open tcp/$new_port manually in the local and provider firewalls" >&2
    return 0
  fi
  if [[ -n "$managed_firewall_backend" && "$managed_firewall_port" != "$new_port" ]]; then
    remove_firewall_rule "$managed_firewall_backend" "$managed_firewall_port"
  fi
  if [[ -n "$added_firewall_backend" ]]; then
    printf '%s %s\n' "$added_firewall_backend" "$new_port" >"$firewall_state_file"
    chmod 0600 "$firewall_state_file"
  else
    rm -f "$firewall_state_file"
  fi
}

print_summary() {
  local port
  port="$(listen_port "$listen_addr")"
  echo
  echo "anytls-server is installed and started."
  echo "service: sudo systemctl status $service_name"
  echo "logs:    sudo journalctl -u $service_name -f"
  echo "port:    $port/tcp must also be open in your cloud firewall/security group."
  [[ -n "$fallback_addr" ]] && echo "fallback: invalid AnyTLS traffic is forwarded to $fallback_addr."
  echo
  echo "Client URI:"
  client_uri
}

doctor_failure=0

doctor_ok() { echo "  ok: $*"; }
doctor_warn() { echo "  warn: $*"; }
doctor_fail() { echo "  fail: $*"; doctor_failure=1; }

check_listen_port() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    if ss -ltn "( sport = :$port )" 2>/dev/null | awk 'NR > 1 { found=1 } END { exit found ? 0 : 1 }'; then
      doctor_ok "tcp/$port is listening"
    else
      doctor_fail "tcp/$port is not listening locally"
    fi
  else
    doctor_warn "ss not found; listening port was not checked"
  fi
}

check_firewall_port() {
  local port="$1"
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    if ufw status 2>/dev/null | grep -Eq "(^|[^0-9])${port}/tcp[[:space:]]+ALLOW"; then
      doctor_ok "ufw allows tcp/$port"
    else
      doctor_warn "ufw does not explicitly allow tcp/$port"
    fi
  elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    if firewall-cmd --query-port="$port/tcp" >/dev/null 2>&1; then
      doctor_ok "firewalld allows tcp/$port"
    else
      doctor_warn "firewalld does not allow tcp/$port"
    fi
  elif command -v iptables >/dev/null 2>&1; then
    if iptables -C INPUT -p tcp --dport "$port" -j ACCEPT >/dev/null 2>&1; then
      doctor_ok "iptables allows tcp/$port"
    else
      doctor_warn "no explicit iptables ACCEPT rule for tcp/$port"
    fi
  else
    doctor_warn "no supported local firewall tool detected"
  fi
}

check_fallback() {
  if [[ -z "$fallback_addr" ]]; then
    doctor_ok "fallback disabled"
    return
  fi
  split_endpoint "$fallback_addr" || { doctor_warn "invalid fallback address $fallback_addr"; return; }
  if command -v ss >/dev/null 2>&1 && [[ "$endpoint_host" == "127.0.0.1" || "$endpoint_host" == "localhost" || "$endpoint_host" == "::1" ]]; then
    if ss -ltn "( sport = :$endpoint_port )" 2>/dev/null | awk 'NR > 1 { found=1 } END { exit found ? 0 : 1 }'; then
      doctor_ok "fallback target $fallback_addr is listening"
    else
      doctor_warn "fallback target $fallback_addr is not listening"
    fi
  else
    echo "  info: fallback target $fallback_addr was not checked locally"
  fi
}

run_doctor() {
  require_systemd
  load_existing_defaults
  doctor_failure=0
  local current_port
  current_port="$(listen_port "$listen_addr" 2>/dev/null || true)"

  echo "AnyTLS deployment doctor"
  echo
  echo "Service:"
  if systemctl is-enabled "$service_name" >/dev/null 2>&1; then
    doctor_ok "$service_name is enabled"
  else
    doctor_fail "$service_name is not enabled"
  fi
  if systemctl is-active "$service_name" >/dev/null 2>&1; then
    doctor_ok "$service_name is active"
  else
    doctor_fail "$service_name is not active"
  fi
  if [[ -x "$bin_path" ]]; then
    doctor_ok "binary exists at $bin_path"
  else
    doctor_fail "binary missing at $bin_path"
  fi
  if [[ -f "$env_file" ]]; then
    doctor_ok "config exists at $env_file"
  else
    doctor_fail "config missing at $env_file"
  fi
  if [[ -r "$password_file" ]]; then
    doctor_ok "password file exists"
  else
    doctor_fail "password file missing or unreadable"
  fi
  if [[ -f "$systemd_unit" ]]; then
    doctor_ok "systemd unit exists"
  else
    doctor_fail "systemd unit missing"
  fi
  if [[ -n "$current_port" ]]; then
    check_listen_port "$current_port"
  else
    doctor_fail "listen port is invalid"
  fi

  echo
  echo "Firewall:"
  if [[ "${ANYTLS_DOCTOR_SKIP_FIREWALL:-0}" == "1" ]]; then
    echo "  info: firewall check deferred until the service transaction is committed"
  else
    [[ -n "$current_port" ]] && check_firewall_port "$current_port"
  fi
  echo
  echo "Fallback:"
  check_fallback
  echo
  echo "Client URI:"
  client_uri
  echo
  echo "Reminder: also open tcp/${current_port:-8443} in your cloud security group or provider firewall."

  if [[ "$doctor_failure" -ne 0 ]]; then
    echo "doctor found critical deployment failures" >&2
    return 1
  fi
}

install_or_update() {
  local stage_only="${ANYTLS_STAGE_ONLY:-}"
  if [[ -z "$stage_only" ]]; then
    require_root
    require_systemd
  fi
  collect_install_inputs || return 1
  validate_inputs || return 1

  local stage_dir stage_owned=1
  if [[ -n "$stage_only" ]]; then
    stage_dir="$stage_only"
    stage_owned=0
    install -d -m 0700 "$stage_dir" || return 1
  else
    stage_dir="$(mktemp -d)" || return 1
  fi
  if ! build_staged_binaries "$stage_dir" || ! write_staged_files "$stage_dir"; then
    [[ "$stage_owned" -eq 1 ]] && rm -rf "$stage_dir"
    return 1
  fi
  if [[ -n "$stage_only" ]]; then
    return
  fi

  if ! ensure_service_user; then
    rm -rf "$stage_dir"
    return 1
  fi
  if ! begin_install_transaction "$stage_dir"; then
    remove_created_service_user
    rm -rf "$stage_dir"
    return 1
  fi
  if ! apply_staged_install "$stage_dir"; then
    if rollback_install; then
      rm -rf "$stage_dir"
    else
      echo "recovery files retained at $stage_dir/backup" >&2
    fi
    transaction_dir=""
    return 1
  fi
  if ! run_anytls_probe "$stage_dir/anytls-client" || ! ANYTLS_DOCTOR_SKIP_FIREWALL=1 run_doctor; then
    if rollback_install; then
      rm -rf "$stage_dir"
    else
      echo "recovery files retained at $stage_dir/backup" >&2
    fi
    transaction_dir=""
    return 1
  fi
  commit_install_transaction
  reconcile_firewall "$(listen_port "$listen_addr")"
  rm -rf "$stage_dir"
  transaction_dir=""
  print_summary
}

update_source_tree() {
  if [[ ! -d "$repo_root/.git" ]]; then
    echo "source directory is not a Git checkout: $repo_root" >&2
    return 1
  fi
  if [[ -n "$(git -C "$repo_root" status --porcelain)" ]]; then
    echo "source tree has tracked or untracked changes; commit, stash, or remove them before update" >&2
    return 1
  fi
  echo "fetching source branch $expected_branch..."
  git -C "$repo_root" fetch origin "$expected_branch:refs/remotes/origin/$expected_branch" || return 1
  local current_branch
  current_branch="$(git -C "$repo_root" branch --show-current)" || return 1
  if [[ "$current_branch" != "$expected_branch" ]]; then
    echo "current branch is '$current_branch', expected '$expected_branch'" >&2
    return 1
  fi
  git -C "$repo_root" merge-base --is-ancestor HEAD "origin/$expected_branch" || {
    echo "local branch has diverged from origin/$expected_branch; refusing automatic update" >&2
    return 1
  }
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
  update_source_tree || return 1
  local target_commit current_commit worktree_dir
  target_commit="$(git -C "$repo_root" rev-parse "origin/$expected_branch")" || return 1
  current_commit="$(git -C "$repo_root" rev-parse HEAD)" || return 1
  if [[ "$target_commit" == "$current_commit" ]]; then
    echo "source is already current; rebuilding installed service"
    install_or_update
    return
  fi

  worktree_dir="$(mktemp -d)" || return 1
  rmdir "$worktree_dir" || return 1
  git -C "$repo_root" worktree add --detach "$worktree_dir" "$target_commit" || return 1
  local stage_dir
  if ! stage_dir="$(mktemp -d)"; then
    git -C "$repo_root" worktree remove --force "$worktree_dir" >/dev/null 2>&1 || true
    return 1
  fi
  local -a staged_args=(install --non-interactive --listen "$listen_addr" --server-name "$server_name" --fallback "$fallback_addr" --branch "$expected_branch")
  [[ "$password_arg_set" -eq 1 ]] && staged_args+=(--password "$password")
  [[ "$auto_firewall" -eq 0 ]] && staged_args+=(--no-firewall)
  [[ -n "$padding_scheme" ]] && staged_args+=(--padding-scheme "$padding_scheme")
  [[ -n "$binary_path" ]] && staged_args+=(--binary "$binary_path")

  local update_status=0
  ANYTLS_UPDATE_STAGED=1 \
    ANYTLS_STAGE_ONLY="$stage_dir" \
    ANYTLS_DEPLOYMENT_LOCK_HELD=1 \
    "$worktree_dir/scripts/install-anytls-server.sh" "${staged_args[@]}" || update_status=$?
  git -C "$repo_root" worktree remove --force "$worktree_dir" >/dev/null 2>&1 || true
  if [[ "$update_status" -ne 0 ]]; then
    rm -rf "$stage_dir"
    echo "candidate staging failed; service and source checkout were not changed" >&2
    return "$update_status"
  fi

  if ! ensure_service_user || ! begin_install_transaction "$stage_dir"; then
    remove_created_service_user
    rm -rf "$stage_dir"
    return 1
  fi
  if ! apply_staged_install "$stage_dir" || \
    ! run_anytls_probe "$stage_dir/anytls-client" || \
    ! ANYTLS_DOCTOR_SKIP_FIREWALL=1 run_doctor; then
    if rollback_install; then
      rm -rf "$stage_dir"
    else
      echo "recovery files retained at $stage_dir/backup" >&2
    fi
    transaction_dir=""
    return 1
  fi

  if [[ "$(git -C "$repo_root" rev-parse HEAD)" != "$current_commit" || -n "$(git -C "$repo_root" status --porcelain)" ]] || \
    ! git -C "$repo_root" merge --ff-only "$target_commit"; then
    echo "source checkout changed while candidate was deploying; rolling service back" >&2
    if rollback_install; then
      rm -rf "$stage_dir"
    else
      echo "recovery files retained at $stage_dir/backup" >&2
    fi
    transaction_dir=""
    return 1
  fi

  commit_install_transaction
  reconcile_firewall "$(listen_port "$listen_addr")"
  rm -rf "$stage_dir"
  transaction_dir=""
  print_summary
}

show_status() {
  require_systemd
  load_existing_defaults
  echo "Service:"
  systemctl --no-pager --full status "$service_name" || true
  echo
  if [[ -f "$env_file" ]]; then
    echo "Config:"
    echo "  listen: $listen_addr"
    echo "  server: $server_name"
    echo "  fallback: $fallback_addr"
    echo "  env: $env_file"
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
  systemctl restart "$service_name" || return 1
  wait_for_service_stable || return 1
  show_status
}

do_uninstall() {
  require_root
  require_systemd
  if [[ "$non_interactive" -ne 1 ]] && ! prompt_yes_no "Uninstall anytls-server and remove managed files?" "n"; then
    echo "cancelled"
    return
  fi
  read_firewall_state
  local remove_service_user=0
  [[ -f "$config_dir/user.managed" ]] && remove_service_user=1
  remove_firewall_rule "$managed_firewall_backend" "$managed_firewall_port"
  systemctl disable --now "$service_name" 2>/dev/null || true
  rm -f "$systemd_unit" "$bin_path" || return 1
  rm -rf "$config_dir" || return 1
  systemctl daemon-reload || return 1
  if [[ "$remove_service_user" -eq 1 ]]; then
    userdel "$service_user" >/dev/null 2>&1 || deluser "$service_user" >/dev/null 2>&1 || true
  fi
  if [[ -f "$repo_root/.anytls-bootstrap-managed" && "$repo_root" != "/" && "$repo_root" != "$HOME" ]]; then
    echo "removing bootstrap-managed source checkout: $repo_root"
    rm -rf "$repo_root" || return 1
  fi
  echo "anytls-server uninstalled. The shared Go toolchain and OS packages were left installed."
}

show_menu() {
  require_root
  require_systemd
  while true; do
    echo
    echo "AnyTLS Server Manager"
    echo "1) Install / Reinstall"
    echo "2) Update $expected_branch and restart"
    echo "3) Status and client URI"
    echo "4) Doctor / self-check"
    echo "5) Restart"
    echo "6) Uninstall"
    echo "0) Exit"
    read -r -p "Select [1]: " choice </dev/tty
    choice="${choice:-1}"
    case "$choice" in
      1) with_deployment_lock install_or_update ;;
      2) with_deployment_lock do_update ;;
      3) show_status ;;
      4) run_doctor ;;
      5) with_deployment_lock do_restart ;;
      6) with_deployment_lock do_uninstall ;;
      0) exit 0 ;;
      *) echo "invalid choice" ;;
    esac
  done
}

if [[ "${ANYTLS_INSTALLER_LIB_ONLY:-0}" != "1" ]]; then
  case "$action" in
    install) with_deployment_lock install_or_update ;;
    update) with_deployment_lock do_update ;;
    status) show_status ;;
    doctor) run_doctor ;;
    restart) with_deployment_lock do_restart ;;
    uninstall) with_deployment_lock do_uninstall ;;
    menu) show_menu ;;
    *) usage >&2; exit 2 ;;
  esac
fi
