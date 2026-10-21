#!/usr/bin/env bash
# Sourced by install-anytls-server.sh; shares its deployment transaction.
# shellcheck disable=SC2154

dedup_service_name="${ANYTLS_DEDUP_SERVICE_NAME:-anytls-dedup}"
dedup_user="${ANYTLS_DEDUP_USER:-anytls-dedup}"
dedup_bin="${ANYTLS_DEDUP_BIN_PATH:-/usr/local/bin/anytls-dedup}"
dedup_unit="${ANYTLS_DEDUP_SYSTEMD_UNIT:-$(dirname "$systemd_unit")/$dedup_service_name.service}"
dedup_config="$config_dir/dedup.env"
dedup_runtime_name="${ANYTLS_DEDUP_RUNTIME_NAME:-$dedup_service_name}"
dedup_runtime="${ANYTLS_DEDUP_RUNTIME_DIR:-/run/$dedup_runtime_name}"
dedup_backend="unix:$dedup_runtime/back.sock"
dedup_requested=""
dedup_enabled=1
dedup_user_created=0
dedup_was_active=0
dedup_was_enabled=0

dedup_option() {
  if [[ -n "$dedup_requested" && "$dedup_requested" != "$1" ]]; then
    echo '--with-dedup and --without-dedup cannot be combined' >&2
    return 1
  fi
  dedup_requested="$1"
}

read_dedup_value() {
  local key="$1"
  [[ -f "$dedup_config" ]] || return 0
  (
    # Root-owned deployment configuration, like server.env.
    # shellcheck disable=SC1090
    source "$dedup_config" || exit 1
    printf '%s' "${!key-}"
  )
}

dedup_load_defaults() {
  local mode="${1:-deploy}" saved old_backend key value
  saved="$(read_env_value ANYTLS_DEDUP_ENABLED)" || return 1
  old_backend="$(read_env_value ANYTLS_DEDUP_BACKEND)" || return 1
  if env_key_exists ANYTLS_DEDUP_ENABLED && [[ "$saved" != 0 && "$saved" != 1 ]]; then
    echo 'invalid saved ANYTLS_DEDUP_ENABLED (expected 0 or 1)' >&2
    return 1
  fi
  if [[ -z "$saved" && -n "$old_backend" && "$mode" == deploy ]]; then
    echo 'unmanaged dedup backend found; migrate its configuration before upgrading' >&2
    return 1
  fi
  if [[ "$mode" == deploy && -n "$saved" && -n "$old_backend" && "$old_backend" != "$dedup_backend" ]]; then
    echo 'saved dedup backend differs from the managed socket; explicit migration is required' >&2
    return 1
  fi
  dedup_enabled="${dedup_requested:-${saved:-1}}"
  [[ "$mode" != read || -n "$saved" ]] || dedup_enabled=0
  dedup_domains='chatgpt.com,api.openai.com'
  dedup_cache=256
  dedup_inflight=64
  dedup_max_body=64
  dedup_memory=448
  dedup_memory_max=500
  for key in DOMAINS CACHE_MIB INFLIGHT_MIB MAX_BODY_MIB MEMORY_MIB MEMORY_MAX_MIB; do
    if [[ -f "$dedup_config" ]] && grep -q "^ANYTLS_DEDUP_${key}=" "$dedup_config"; then
      value="$(read_dedup_value "ANYTLS_DEDUP_$key")" || return 1
      case "$key" in
        DOMAINS) dedup_domains="$value" ;;
        CACHE_MIB) dedup_cache="$value" ;;
        INFLIGHT_MIB) dedup_inflight="$value" ;;
        MAX_BODY_MIB) dedup_max_body="$value" ;;
        MEMORY_MIB) dedup_memory="$value" ;;
        MEMORY_MAX_MIB) dedup_memory_max="$value" ;;
      esac
    fi
  done
}

dedup_validate() {
  local n
  for n in "$dedup_service_name" "$dedup_runtime_name" "$dedup_user"; do
    [[ "$n" =~ ^[a-zA-Z_][a-zA-Z0-9_-]*$ ]] || { echo "invalid dedup identity: $n" >&2; return 1; }
  done
  for n in "$dedup_bin" "$dedup_unit" "$dedup_runtime"; do
    [[ "$n" =~ ^/[a-zA-Z0-9_./-]+$ && "$n" != *'/../'* && "$n" != */.. && "$n" != / ]] || return 1
  done
  [[ "$dedup_service_name" != "$service_name" && "$dedup_bin" != "$bin_path" && "$dedup_unit" != "$systemd_unit" ]] || return 1
  for n in "$dedup_cache" "$dedup_inflight" "$dedup_max_body" "$dedup_memory" "$dedup_memory_max"; do
    [[ "$n" =~ ^[1-9][0-9]{0,5}$ ]] || { echo 'invalid dedup memory/body budget' >&2; return 1; }
  done
  (( dedup_memory >= dedup_cache * 5 / 4 + dedup_inflight + 64 && dedup_memory_max > dedup_memory )) || {
    echo 'dedup memory limits must cover cache overhead, inflight budget and runtime headroom' >&2
    return 1
  }
  [[ "$dedup_domains" =~ ^[a-zA-Z0-9][a-zA-Z0-9.,-]*$ ]] || { echo 'dedup domain allowlist must be nonempty' >&2; return 1; }
  python3 - "$dedup_domains" <<'PY'
import re, sys
for host in sys.argv[1].split(','):
    if len(host) > 253 or '.' not in host or any(not re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?', label) for label in host.split('.')):
        raise SystemExit('invalid dedup domain allowlist')
PY
}

dedup_preflight() {
  dedup_validate || return 1
  # Unit overrides could silently remove resource limits or change endpoints.
  local name
  for name in "$service_name" "$dedup_service_name"; do
    if [[ -n "$(systemctl show "$name" -p DropInPaths --value 2>/dev/null)" ]]; then
      echo "custom systemd drop-ins for $name require explicit migration" >&2
      return 1
    fi
  done
  [[ "$dedup_enabled" == 1 ]] || return 0
  local total available
  total="$(awk '/^MemTotal:/ {print int($2/1024)}' /proc/meminfo)"
  available="$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo)"
  echo "dedup: enabled; domains=$dedup_domains; hard limit=${dedup_memory_max} MiB; host total=$total MiB available=$available MiB"
  echo 'dedup: use --without-dedup to opt out; client CA and device selection remain managed by the client.'
  if (( total < dedup_memory_max + 128 || available < dedup_inflight + 64 )); then
    echo 'insufficient host memory for the recommended dedup profile' >&2
    return 1
  fi
  if [[ -e "$dedup_runtime" || -L "$dedup_runtime" ]]; then
    [[ ! -L "$dedup_runtime" && -d "$dedup_runtime" ]] || { echo 'unsafe dedup runtime directory' >&2; return 1; }
    [[ "$(stat -c %a "$dedup_runtime")" == 750 ]] || { echo 'dedup runtime directory must have mode 0750' >&2; return 1; }
    [[ "$(stat -c %U "$dedup_runtime")" == "$dedup_user" && "$(stat -c %G "$dedup_runtime")" == "$service_group" ]] || {
      echo 'dedup runtime directory has unexpected ownership' >&2; return 1;
    }
    for name in back admin; do
      if [[ -e "$dedup_runtime/$name.sock" || -L "$dedup_runtime/$name.sock" ]]; then
        [[ ! -L "$dedup_runtime/$name.sock" && -S "$dedup_runtime/$name.sock" ]] || { echo 'unsafe dedup socket path' >&2; return 1; }
      fi
    done
  fi
}

dedup_ensure_user() {
  dedup_user_created=0
  [[ "$dedup_enabled" == 1 ]] || return 0
  if id "$dedup_user" >/dev/null 2>&1; then
    [[ "$(id -gn "$dedup_user")" == "$service_group" ]] || { echo 'dedup user has an unexpected primary group' >&2; return 1; }
    return
  fi
  useradd --system --gid "$service_group" --home-dir /nonexistent --shell /usr/sbin/nologin "$dedup_user" || return 1
  dedup_user_created=1
}

dedup_remove_created_user() {
  if [[ "$dedup_user_created" == 1 ]]; then
    userdel "$dedup_user" || return 1
    dedup_user_created=0
  fi
}

dedup_build() {
  local stage_dir="$1"
  [[ "$dedup_enabled" == 1 ]] || return 0
  local help
  help="$("$stage_dir/anytls-server" -h 2>&1 || true)"
  if ! grep -q -- '-dedup-backend' <<<"$help"; then
    echo 'candidate AnyTLS binary does not support dedup-backend' >&2
    return 1
  fi
  (cd "$source_root" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$stage_dir/anytls-dedup" ./cmd/dedup)
}

dedup_write_staged() {
  local stage_dir="$1"
  printf 'ANYTLS_DEDUP_ENABLED=%s\nANYTLS_DEDUP_BACKEND=%s\n' \
    "$dedup_enabled" "$(env_quote "$( [[ "$dedup_enabled" == 1 ]] && printf '%s' "$dedup_backend" )")" >>"$stage_dir/server.env" || return 1
  [[ "$dedup_enabled" == 1 ]] || return 0
  if ! cat >"$stage_dir/dedup.env" <<EOF
ANYTLS_DEDUP_DOMAINS=$(env_quote "$dedup_domains")
ANYTLS_DEDUP_CACHE_MIB=$dedup_cache
ANYTLS_DEDUP_INFLIGHT_MIB=$dedup_inflight
ANYTLS_DEDUP_MAX_BODY_MIB=$dedup_max_body
ANYTLS_DEDUP_MEMORY_MIB=$dedup_memory
ANYTLS_DEDUP_MEMORY_MAX_MIB=$dedup_memory_max
EOF
  then return 1; fi
  if ! cat >"$stage_dir/$dedup_service_name.service" <<EOF
[Unit]
Description=AnyTLS request deduplication decoder
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=$dedup_user
Group=$service_group
EnvironmentFile=$dedup_config
ExecStart=$dedup_bin back -listen $dedup_backend -admin unix:$dedup_runtime/admin.sock -domains \$ANYTLS_DEDUP_DOMAINS -cache-mib \$ANYTLS_DEDUP_CACHE_MIB -inflight-mib \$ANYTLS_DEDUP_INFLIGHT_MIB -max-body-mib \$ANYTLS_DEDUP_MAX_BODY_MIB -memory-mib \$ANYTLS_DEDUP_MEMORY_MIB -drain 30s
RuntimeDirectory=$dedup_runtime_name
RuntimeDirectoryMode=0750
UMask=0007
Restart=on-failure
RestartSec=1
TimeoutStopSec=40
MemoryMax=${dedup_memory_max}M
MemorySwapMax=0
OOMPolicy=kill
OOMScoreAdjust=500
LimitNOFILE=65536
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectControlGroups=true
ProtectKernelModules=true
ProtectKernelTunables=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF
  then return 1; fi
  chmod 0640 "$stage_dir/dedup.env" || return 1
  chmod 0644 "$stage_dir/$dedup_service_name.service" || return 1
}

dedup_backup() {
  dedup_was_active=0
  dedup_was_enabled=0
  systemctl is-active "$dedup_service_name" >/dev/null 2>&1 && dedup_was_active=1
  systemctl is-enabled "$dedup_service_name" >/dev/null 2>&1 && dedup_was_enabled=1
  backup_path "$dedup_bin" dedup_binary &&
    backup_path "$dedup_config" dedup_environment &&
    backup_path "$dedup_unit" dedup_unit &&
    backup_path "$config_dir/dedup-user.managed" dedup_user_managed
}

dedup_local_request() {
  local socket="$1" endpoint="$2"
  if [[ -n "${ANYTLS_DEDUP_HEALTH_COMMAND:-}" ]]; then
    "$ANYTLS_DEDUP_HEALTH_COMMAND" "$socket" "$endpoint"
    return
  fi
  python3 - "$socket" "$endpoint" <<'PY'
import http.client, json, re, socket, sys
class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(sys.argv[1])
try:
    c = UnixHTTP('dedup.anytls.arpa', timeout=5)
    try:
        c.request('GET', sys.argv[2])
        r = c.getresponse()
        if sys.argv[2] == '/health':
            epoch = r.getheader('X-Edge-Dedup-Epoch', '')
            if r.status != 204 or not re.fullmatch(r'[0-9a-f]{16}', epoch):
                raise ValueError('invalid decoder health response')
            print(epoch)
        else:
            if r.status != 200: raise ValueError('decoder statistics unavailable')
            data = r.read(65537)
            if len(data) > 65536: raise ValueError('oversized decoder statistics')
            print(json.dumps(json.loads(data)))
    finally:
        c.close()
except (OSError, ValueError, http.client.HTTPException) as e:
    raise SystemExit(str(e))
PY
}

dedup_check_local() {
  [[ "$dedup_enabled" == 1 ]] || return 0
  systemctl is-active "$dedup_service_name" >/dev/null 2>&1 || return 1
  systemctl is-enabled "$dedup_service_name" >/dev/null 2>&1 || return 1
  [[ "$(systemctl show "$dedup_service_name" -p MemoryMax --value)" == "$((dedup_memory_max * 1024 * 1024))" ]] || {
    echo 'decoder effective MemoryMax differs from the configured hard limit' >&2; return 1;
  }
  local cg limit_file
  cg="$(systemctl show "$dedup_service_name" -p ControlGroup --value)" || return 1
  [[ "$cg" == /* && "$cg" != *..* ]] || return 1
  limit_file="${ANYTLS_CGROUP_ROOT:-/sys/fs/cgroup}$cg/memory.max"
  [[ -r "$limit_file" ]] || limit_file="${ANYTLS_CGROUP_ROOT:-/sys/fs/cgroup}/memory$cg/memory.limit_in_bytes"
  if [[ ! -r "$limit_file" || "$(cat "$limit_file")" != "$((dedup_memory_max * 1024 * 1024))" ]]; then
    echo 'decoder hard memory limit is not enforced by its cgroup' >&2
    return 1
  fi
  dedup_local_request "$dedup_runtime/back.sock" /health
}

dedup_apply() {
  local stage_dir="$1"
  [[ "$dedup_enabled" == 1 ]] || return 0
  atomic_install "$stage_dir/anytls-dedup" "$dedup_bin" root root 0755 &&
    atomic_install "$stage_dir/dedup.env" "$dedup_config" root "$service_group" 0640 &&
    atomic_install "$stage_dir/$dedup_service_name.service" "$dedup_unit" root root 0644 || return 1
  if [[ "$dedup_user_created" == 1 ]]; then
    install -m 0600 /dev/null "$config_dir/dedup-user.managed" || return 1
  fi
  systemctl daemon-reload && systemctl enable "$dedup_service_name" && systemctl restart "$dedup_service_name" || return 1
  wait_for_service_stable "$dedup_service_name" || return 1
  dedup_check_local >/dev/null
}

dedup_finish_apply() {
  [[ "$dedup_enabled" == 0 ]] || return 0
  if [[ -f "$dedup_unit" ]]; then
    systemctl stop "$dedup_service_name" && systemctl disable "$dedup_service_name" || return 1
  fi
}

dedup_probe() {
  local probe_binary="$1" local_epoch remote_epoch
  [[ "$dedup_enabled" == 1 ]] || return 0
  local_epoch="$(dedup_check_local)" || return 1
  if [[ -n "${ANYTLS_DEDUP_PROBE_COMMAND:-}" ]]; then
    remote_epoch="$("$ANYTLS_DEDUP_PROBE_COMMAND" "$local_epoch")" || return 1
  else
    remote_epoch="$(LOG_LEVEL=warn "$probe_binary" -probe-dedup -s "$(probe_server_address)" -password-file "$password_file" -insecure=true -m 0)" || return 1
  fi
  [[ "$local_epoch" == "$remote_epoch" ]] || { echo 'AnyTLS decoder probe reached a different decoder epoch' >&2; return 1; }
}

dedup_restore_files() {
  local failed=0
  if [[ -f "$dedup_unit" ]]; then
    systemctl stop "$dedup_service_name" >/dev/null 2>&1 || failed=1
    systemctl disable "$dedup_service_name" >/dev/null 2>&1 || failed=1
  fi
  restore_path "$dedup_bin" dedup_binary || failed=1
  restore_path "$dedup_config" dedup_environment || failed=1
  restore_path "$dedup_unit" dedup_unit || failed=1
  restore_path "$config_dir/dedup-user.managed" dedup_user_managed || failed=1
  return "$failed"
}

dedup_restore_state() {
  local failed=0
  if [[ "$dedup_was_enabled" == 1 ]]; then
    systemctl enable "$dedup_service_name" >/dev/null 2>&1 || failed=1
  elif [[ -f "$dedup_unit" ]]; then
    systemctl disable "$dedup_service_name" >/dev/null 2>&1 || failed=1
  fi
  if [[ "$dedup_was_active" == 1 ]]; then
    systemctl restart "$dedup_service_name" >/dev/null 2>&1 && wait_for_service_stable "$dedup_service_name" || failed=1
  fi
  dedup_remove_created_user || failed=1
  return "$failed"
}


dedup_finalize_restore_state() {
  # Restarting a restored main unit may activate its Wants= decoder again.
  if [[ "$dedup_was_active" == 0 ]] && { [[ -f "$dedup_unit" ]] || systemctl is-active "$dedup_service_name" >/dev/null 2>&1; }; then
    systemctl stop "$dedup_service_name" >/dev/null 2>&1 || return 1
    ! systemctl is-active "$dedup_service_name" >/dev/null 2>&1 || return 1
  fi
}

dedup_status() {
  echo "Dedup enabled: $dedup_enabled"
  [[ "$dedup_enabled" == 1 ]] || return 0
  echo "Dedup domains: $dedup_domains; cache=$dedup_cache MiB; soft=$dedup_memory MiB; hard=$dedup_memory_max MiB"
  systemctl --no-pager --full status "$dedup_service_name" || true
  if ! dedup_local_request "$dedup_runtime/admin.sock" /stats; then
    echo 'Dedup statistics unavailable; client-side savings have not been verified.'
  fi
}

dedup_doctor() {
  if [[ "$dedup_enabled" == 0 ]]; then
    if [[ -f "$dedup_unit" ]] && { systemctl is-active "$dedup_service_name" >/dev/null 2>&1 || systemctl is-enabled "$dedup_service_name" >/dev/null 2>&1; }; then
      doctor_fail 'disabled decoder still running or enabled at boot'
    fi
    if [[ -n "$(read_env_value ANYTLS_DEDUP_BACKEND)" ]]; then
      doctor_fail 'disabled decoder still configured as a backend'
    else
      doctor_ok 'dedup disabled'
    fi
    return
  fi
  if [[ "$(read_env_value ANYTLS_DEDUP_BACKEND)" != "$dedup_backend" || ! -x "$dedup_bin" || ! -f "$dedup_config" || ! -f "$dedup_unit" ]]; then
    doctor_fail 'decoder program, files or main-service backend configuration missing'
    return
  fi
  if dedup_validate && dedup_check_local >/dev/null; then
    doctor_ok 'decoder is enabled, healthy and has the configured hard memory limit'
  else
    doctor_fail 'decoder configuration, service, health or memory limit failed'
  fi
}
