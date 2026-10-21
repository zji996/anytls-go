#!/usr/bin/env bash
set -euo pipefail
# Tests are independent of the production candidate coordinator invoking them.
unset ANYTLS_UPDATE_REPO_ROOT ANYTLS_UPDATE_COMMIT ANYTLS_STAGE_ONLY ANYTLS_UPDATE_STAGED ANYTLS_DEPLOYMENT_LOCK_HELD

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
installer="$script_dir/install-anytls-server.sh"
test_root="$(mktemp -d)"
cleanup_tests() {
  local status=$?
  if [[ "$status" != 0 ]]; then
    echo "installation tests failed (status $status)" >&2
    for log in "$test_root/dedup-install.log" "$test_root/dedup-doctor.log" "$test_root/expected-failure.log"; do
      [[ ! -f "$log" ]] || tail -35 "$log" >&2
    done
  fi
  rm -rf "$test_root"
}
trap cleanup_tests EXIT
# Report the failed assertion without printing commands or credentials.
trap 'if [[ $- == *e* ]]; then echo "installation test failed at ${BASH_SOURCE[0]}:$LINENO" >&2; fi' ERR

# Prevent mock uninstall test from wiping out the actual repository
export ANYTLS_TEST_MODE=1

# shellcheck disable=SC1090
ANYTLS_INSTALLER_LIB_ONLY=1 source "$installer"
# shellcheck disable=SC2317,SC2329
command() {
  if [[ "${1:-}" == "-v" && "${2:-}" == "openssl" ]]; then
    return 1
  fi
  builtin command "$@"
}
generated_password="$(generate_password)"
unset -f command
[[ ${#generated_password} -ge 43 ]]
[[ "$(uri_encode 'a b/@中')" == 'a%20b%2F%40%E4%B8%AD' ]]
[[ "$(normalize_uri_host 'fe80::1%eth0')" == '[fe80::1%25eth0]' ]]
split_endpoint '[2001:db8::1]:443'
if split_endpoint '2001:db8::1:443'; then
  echo "unbracketed IPv6 endpoint should be rejected" >&2
  exit 1
fi

mock_bin="$test_root/mock-bin"
mkdir -p "$mock_bin"

real_id="$(command -v id)"
real_install="$(command -v install)"
real_go="$(command -v go)"
real_cp="$(command -v cp)"
real_git="$(command -v git)"

cat >"$mock_bin/id" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == "-u" && $# -eq 1 ]]; then
  echo 0
  exit 0
fi
exec "$REAL_ID" "$@"
EOF

cat >"$mock_bin/install" <<'EOF'
#!/usr/bin/env bash
args=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o|-g) shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done
target="${args[${#args[@]}-1]}"
if [[ -f "${MOCK_INSTALL_FAIL_MATCH_FILE:-}" ]]; then
  fail_match="$(<"$MOCK_INSTALL_FAIL_MATCH_FILE")"
  if [[ -n "$fail_match" && "$target" == *"$fail_match"* ]]; then
    exit 1
  fi
fi
exec "$REAL_INSTALL" "${args[@]}"
EOF

cat >"$mock_bin/go" <<'EOF'
#!/usr/bin/env bash
[[ ! -f "$MOCK_GO_FAIL" ]] || exit 1
exec "$REAL_GO" "$@"
EOF

cat >"$mock_bin/cp" <<'EOF'
#!/usr/bin/env bash
target="${!#}"
if [[ -f "$MOCK_CP_FAIL" && "$target" == *'/backup/environment' ]]; then
  exit 1
fi
exec "$REAL_CP" "$@"
EOF

cat >"$mock_bin/git" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${MOCK_GIT_FAIL_FETCH_ROOT:-}" && "$*" == "-C $MOCK_GIT_FAIL_FETCH_ROOT fetch "* ]]; then
  echo 'injected fetch failure' >&2
  exit 42
fi
if [[ -n "${MOCK_GIT_INTERRUPT_ROOT:-}" && "$*" == "-C $MOCK_GIT_INTERRUPT_ROOT merge --ff-only "* ]]; then
  "$REAL_GIT" "$@" || exit $?
  kill -TERM "$PPID"
  exit 0
fi
exec "$REAL_GIT" "$@"
EOF
chmod +x "$mock_bin/git"
cat >"$mock_bin/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$MOCK_SYSTEMCTL_LOG"
target="${!#}"
if [[ "$target" == "${ANYTLS_DEDUP_SERVICE_NAME:-anytls-dedup}" ]]; then
  MOCK_SYSTEMCTL_ACTIVE="$MOCK_SYSTEMCTL_ACTIVE.dedup"
  MOCK_SYSTEMCTL_ENABLED="$MOCK_SYSTEMCTL_ENABLED.dedup"
fi
case "${1:-}" in
  is-active) [[ -f "$MOCK_SYSTEMCTL_ACTIVE" ]] ;;
  is-enabled) [[ -f "$MOCK_SYSTEMCTL_ENABLED" ]] ;;
  show)
    case "$*" in
      *DropInPaths*) exit 0 ;;
      *MemoryMax*) echo 524288000 ;;
      *ControlGroup*) echo /mock ;;
      *) echo 4242 ;;
    esac
    ;;
  enable) touch "$MOCK_SYSTEMCTL_ENABLED" ;;
  disable)
    rm -f "$MOCK_SYSTEMCTL_ENABLED"
    [[ "$*" == *"--now"* ]] && rm -f "$MOCK_SYSTEMCTL_ACTIVE"
    exit 0
    ;;
  restart)
    if [[ "$target" == "${ANYTLS_DEDUP_SERVICE_NAME:-anytls-dedup}" && -f "${MOCK_DEDUP_FAIL_RESTART_ONCE:-}" ]]; then
      rm -f "$MOCK_DEDUP_FAIL_RESTART_ONCE"
      exit 1
    fi
    if [[ -f "$MOCK_SYSTEMCTL_FAIL_RESTART_ONCE" ]]; then
      rm -f "$MOCK_SYSTEMCTL_FAIL_RESTART_ONCE"
      exit 1
    fi
    [[ ! -f "$MOCK_SYSTEMCTL_FAIL_RESTART" ]] || exit 1
    touch "$MOCK_SYSTEMCTL_ACTIVE"
    if [[ "$target" == "${ANYTLS_SERVICE_NAME:-}" ]] && grep -Fq "Wants=${ANYTLS_DEDUP_SERVICE_NAME:-}.service" "$ANYTLS_SYSTEMD_UNIT"; then
      touch "$MOCK_SYSTEMCTL_ACTIVE.dedup"
    fi
    ;;
  stop)
    if [[ "$target" == "${ANYTLS_DEDUP_SERVICE_NAME:-anytls-dedup}" && -f "${MOCK_DEDUP_FAIL_STOP_ONCE:-}" ]]; then
      rm -f "$MOCK_DEDUP_FAIL_STOP_ONCE"
      exit 1
    fi
    rm -f "$MOCK_SYSTEMCTL_ACTIVE"
    ;;
  *) exit 0 ;;
esac
EOF

cat >"$mock_bin/ss" <<'EOF'
#!/usr/bin/env bash
echo 'State Recv-Q Send-Q Local Address:Port Peer Address:Port'
echo 'LISTEN 0 4096 0.0.0.0:8443 0.0.0.0:*'
EOF

cat >"$mock_bin/journalctl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat >"$mock_bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat >"$mock_bin/probe" <<'EOF'
#!/usr/bin/env bash
printf '%s %s\n' "$1" "$2" >>"$MOCK_PROBE_LOG"
if [[ -n "${MOCK_PROBE_TOUCH_FILE:-}" ]]; then
  touch "$MOCK_PROBE_TOUCH_FILE"
fi
[[ ! -f "$MOCK_PROBE_FAIL" ]]
EOF
cat >"$mock_bin/iptables" <<'EOF'
#!/usr/bin/env bash
port=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "--dport" ]]; then
    port="$2"
    shift 2
  else
    action="${action:-$1}"
    shift
  fi
done
rule="$MOCK_FIREWALL_RULE_DIR/$port"
case "$action" in
  -C) [[ -f "$rule" ]] ;;
  -I)
    [[ ! -f "$MOCK_FIREWALL_FAIL_ADD" ]] || exit 1
    touch "$rule"
    ;;
  -D) rm -f "$rule" ;;
esac
EOF
# Always shadow the other supported firewalls. Otherwise an active host UFW
# or firewalld takes priority over mocked iptables, even with --no-firewall
# on the installer calls: the rule-reconciliation assertions call it directly.
cat >"$mock_bin/ufw" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == status ]]; then
  echo 'Status: inactive'
  exit 0
fi
echo 'unexpected UFW mutation in isolated installation test' >&2
exit 1
EOF
cat >"$mock_bin/firewall-cmd" <<'EOF'
#!/usr/bin/env bash
[[ "${1:-}" == --state ]] || echo 'unexpected firewalld mutation in isolated installation test' >&2
exit 1
EOF
chmod +x "$mock_bin/id" "$mock_bin/install" "$mock_bin/go" "$mock_bin/cp" "$mock_bin/systemctl" "$mock_bin/ss" "$mock_bin/journalctl" "$mock_bin/sleep" "$mock_bin/probe" "$mock_bin/iptables" "$mock_bin/ufw" "$mock_bin/firewall-cmd"

old_binary="$test_root/old-server"
new_binary="$test_root/new-server"
printf '#!/usr/bin/env bash\necho old\n' >"$old_binary"
printf '#!/usr/bin/env bash\necho new\n' >"$new_binary"
chmod +x "$old_binary" "$new_binary"

export PATH="$mock_bin:$PATH"
export REAL_ID="$real_id"
export REAL_INSTALL="$real_install"
export REAL_GO="$real_go"
export REAL_CP="$real_cp"
export REAL_GIT="$real_git"
export MOCK_SYSTEMCTL_LOG="$test_root/systemctl.log"
export MOCK_SYSTEMCTL_FAIL_RESTART="$test_root/fail-restart"
export MOCK_SYSTEMCTL_FAIL_RESTART_ONCE="$test_root/fail-restart-once"
export MOCK_SYSTEMCTL_ACTIVE="$test_root/systemctl.active"
export MOCK_SYSTEMCTL_ENABLED="$test_root/systemctl.enabled"
export MOCK_INSTALL_FAIL_MATCH_FILE="$test_root/install-fail-match"
export MOCK_GO_FAIL="$test_root/go-fail"
export MOCK_CP_FAIL="$test_root/cp-fail"
export MOCK_PROBE_FAIL="$test_root/probe-fail"
export MOCK_PROBE_LOG="$test_root/probe.log"
export MOCK_FIREWALL_RULE_DIR="$test_root/firewall-rules"
export MOCK_FIREWALL_FAIL_ADD="$test_root/firewall-fail-add"
export ANYTLS_HEALTH_PROBE_COMMAND="$mock_bin/probe"
export ANYTLS_SKIP_CANDIDATE_TESTS=1
export ANYTLS_SERVICE_NAME="anytls-test"
export ANYTLS_SERVICE_USER="root"
export ANYTLS_SERVICE_GROUP="root"
export ANYTLS_CONFIG_DIR="$test_root/etc/anytls"
export ANYTLS_ENV_FILE="$ANYTLS_CONFIG_DIR/server.env"
export ANYTLS_PASSWORD_FILE="$ANYTLS_CONFIG_DIR/server.password"
export ANYTLS_FIREWALL_STATE_FILE="$ANYTLS_CONFIG_DIR/firewall.state"
export ANYTLS_BIN_PATH="$test_root/usr/local/bin/anytls-server"
export ANYTLS_PROBE_BIN_PATH="$test_root/usr/local/libexec/anytls-health-probe"
export ANYTLS_SYSTEMD_UNIT="$test_root/etc/systemd/system/anytls-test.service"
export ANYTLS_CGROUP_ROOT="$test_root/cgroup"
mkdir -p "$ANYTLS_CGROUP_ROOT/mock"
printf '%s\n' 524288000 >"$ANYTLS_CGROUP_ROOT/mock/memory.max"
export ANYTLS_DEDUP_SERVICE_NAME=anytls-test-dedup
export ANYTLS_DEDUP_USER=root
export ANYTLS_DEDUP_BIN_PATH="$test_root/usr/local/bin/anytls-dedup"
export ANYTLS_DEDUP_RUNTIME_DIR="$test_root/run/anytls-test-dedup"
export ANYTLS_DEDUP_SYSTEMD_UNIT="$test_root/etc/systemd/system/anytls-test-dedup.service"
export ANYTLS_DEPLOYMENT_LOCK_FILE="$test_root/anytls-test.install.lock"
mkdir -p "$(dirname "$ANYTLS_BIN_PATH")" "$(dirname "$ANYTLS_SYSTEMD_UNIT")"
mkdir -p "$MOCK_FIREWALL_RULE_DIR"

install_args=(
  install --non-interactive --no-firewall --without-dedup
  --password 'secret /@value'
  --listen '0.0.0.0:8443'
  --server-name '2001:db8::1'
  --fallback ''
)

"$installer" "${install_args[@]}" --binary "$old_binary" >/dev/null

cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
[[ -f "$MOCK_SYSTEMCTL_ACTIVE" ]]
[[ -f "$MOCK_SYSTEMCTL_ENABLED" ]]
[[ "$(<"$ANYTLS_PASSWORD_FILE")" == 'secret /@value' ]]
if grep -Fq 'secret /@value' "$ANYTLS_ENV_FILE"; then exit 1; fi
if grep -Fq 'secret /@value' "$ANYTLS_SYSTEMD_UNIT"; then exit 1; fi
grep -Fq "EnvironmentFile=$ANYTLS_ENV_FILE" "$ANYTLS_SYSTEMD_UNIT"
grep -Fq "ExecStart=$ANYTLS_BIN_PATH" "$ANYTLS_SYSTEMD_UNIT"
grep -Fq 'User=root' "$ANYTLS_SYSTEMD_UNIT"
grep -Fq '127.0.0.1:8443' "$MOCK_PROBE_LOG"

"$installer" doctor >/dev/null

"$installer" install --non-interactive --no-firewall --binary "$new_binary" >/dev/null
cmp -s "$new_binary" "$ANYTLS_BIN_PATH"
[[ "$(<"$ANYTLS_PASSWORD_FILE")" == 'secret /@value' ]]
grep -Fq "ANYTLS_LISTEN='0.0.0.0:8443'" "$ANYTLS_ENV_FILE"

rm -f "$ANYTLS_PASSWORD_FILE"
printf '%s\n' "ANYTLS_PASSWORD='legacy-secret'" >>"$ANYTLS_ENV_FILE"
"$installer" install --non-interactive --no-firewall --binary "$old_binary" >/dev/null
[[ "$(<"$ANYTLS_PASSWORD_FILE")" == 'legacy-secret' ]]
if grep -Fq 'ANYTLS_PASSWORD=' "$ANYTLS_ENV_FILE"; then exit 1; fi

touch "$ANYTLS_CONFIG_DIR/user.managed"
restart_count="$(grep -c '^restart ' "$MOCK_SYSTEMCTL_LOG")"
touch "$MOCK_GO_FAIL"
if "$installer" install --non-interactive --no-firewall --binary "$new_binary" >/dev/null 2>&1; then
  echo "expected candidate build failure" >&2
  exit 1
fi
rm -f "$MOCK_GO_FAIL"
cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
[[ "$(grep -c '^restart ' "$MOCK_SYSTEMCTL_LOG")" == "$restart_count" ]]

touch "$MOCK_CP_FAIL"
if "$installer" install --non-interactive --no-firewall --binary "$new_binary" >/dev/null 2>&1; then
  echo "expected transaction backup failure" >&2
  exit 1
fi
rm -f "$MOCK_CP_FAIL"
cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
[[ "$(grep -c '^restart ' "$MOCK_SYSTEMCTL_LOG")" == "$restart_count" ]]

touch "$MOCK_SYSTEMCTL_FAIL_RESTART_ONCE"
if "$installer" install --non-interactive --no-firewall --binary "$new_binary" >/dev/null 2>&1; then
  echo "expected failed restart to fail installation" >&2
  exit 1
fi
cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
[[ -f "$MOCK_SYSTEMCTL_ACTIVE" ]]
[[ -f "$ANYTLS_CONFIG_DIR/user.managed" ]]

touch "$MOCK_PROBE_FAIL"
if "$installer" install --non-interactive --no-firewall --binary "$new_binary" >/dev/null 2>&1; then
  echo "expected failed health probe to fail installation" >&2
  exit 1
fi
rm -f "$MOCK_PROBE_FAIL"
cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
[[ -f "$ANYTLS_CONFIG_DIR/user.managed" ]]

printf '%s' 'server.env' >"$MOCK_INSTALL_FAIL_MATCH_FILE"
if "$installer" install --non-interactive --no-firewall --binary "$new_binary" >/dev/null 2>&1; then
  echo "expected staged file replacement failure" >&2
  exit 1
fi
rm -f "$MOCK_INSTALL_FAIL_MATCH_FILE"
cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
[[ -f "$ANYTLS_CONFIG_DIR/user.managed" ]]

exec 9>"$ANYTLS_DEPLOYMENT_LOCK_FILE"
flock -n 9
set +e
"$installer" install --non-interactive --no-firewall --binary "$new_binary" >/dev/null 2>&1
lock_status=$?
set -e
flock -u 9
exec 9>&-
[[ "$lock_status" -eq 75 ]]
cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
rm -f "$ANYTLS_CONFIG_DIR/user.managed"

# These globals belong to the sourced installer functions.
# shellcheck disable=SC2034
firewall_state_file="$ANYTLS_FIREWALL_STATE_FILE"
printf '%s\n' 'iptables 8443' >"$ANYTLS_FIREWALL_STATE_FILE"
touch "$MOCK_FIREWALL_RULE_DIR/8443" "$MOCK_FIREWALL_FAIL_ADD"
# shellcheck disable=SC2034
auto_firewall=1
reconcile_firewall 9443 >/dev/null 2>&1
[[ "$(<"$ANYTLS_FIREWALL_STATE_FILE")" == 'iptables 8443' ]]
[[ -f "$MOCK_FIREWALL_RULE_DIR/8443" ]]
[[ ! -f "$MOCK_FIREWALL_RULE_DIR/9443" ]]
rm -f "$MOCK_FIREWALL_FAIL_ADD"
reconcile_firewall 9443 >/dev/null
[[ "$(<"$ANYTLS_FIREWALL_STATE_FILE")" == 'iptables 9443' ]]
[[ ! -f "$MOCK_FIREWALL_RULE_DIR/8443" ]]
[[ -f "$MOCK_FIREWALL_RULE_DIR/9443" ]]

"$installer" uninstall --non-interactive >/dev/null
[[ ! -e "$ANYTLS_BIN_PATH" ]]
[[ ! -e "$ANYTLS_CONFIG_DIR" ]]
[[ ! -e "$ANYTLS_SYSTEMD_UNIT" ]]

echo "installation flow tests passed"

# Decoder tests share the real installer and independent per-service mocks.
cat >"$mock_bin/dedup-health" <<'EOF'
#!/usr/bin/env bash
[[ ! -f "$MOCK_DEDUP_HEALTH_FAIL" ]] || exit 1
if [[ "$2" == /health ]]; then
  echo 0123456789abcdef
else
  echo '{"role":"decode","framed":0}'
fi
EOF
cat >"$mock_bin/dedup-probe" <<'EOF'
#!/usr/bin/env bash
[[ ! -f "$MOCK_DEDUP_PROBE_FAIL" ]] || exit 1
if [[ -f "$MOCK_DEDUP_EPOCH_MISMATCH" ]]; then
  echo fedcba9876543210fedcba9876543210
else
  echo "$1"
fi
EOF
chmod +x "$mock_bin/dedup-health" "$mock_bin/dedup-probe"
export ANYTLS_DEDUP_HEALTH_COMMAND="$mock_bin/dedup-health"
export ANYTLS_DEDUP_PROBE_COMMAND="$mock_bin/dedup-probe"
export MOCK_DEDUP_HEALTH_FAIL="$test_root/dedup-health-fail"
export MOCK_DEDUP_PROBE_FAIL="$test_root/dedup-probe-fail"
export MOCK_DEDUP_EPOCH_MISMATCH="$test_root/dedup-epoch-mismatch"
export MOCK_DEDUP_FAIL_RESTART_ONCE="$test_root/dedup-restart-fail-once"
export MOCK_DEDUP_FAIL_STOP_ONCE="$test_root/dedup-stop-fail-once"
dedup_args=(install --non-interactive --no-firewall --password decoder-secret --server-name 127.0.0.1 --fallback '')
"$installer" "${dedup_args[@]}" >"$test_root/dedup-install.log" 2>&1
[[ -f "$MOCK_SYSTEMCTL_ACTIVE" && -f "$MOCK_SYSTEMCTL_ACTIVE.dedup" ]]
[[ -f "$MOCK_SYSTEMCTL_ENABLED.dedup" ]]
grep -q '^ANYTLS_DEDUP_ENABLED=1$' "$ANYTLS_ENV_FILE"
grep -Fq 'MemoryMax=500M' "$ANYTLS_DEDUP_SYSTEMD_UNIT"
grep -Fq 'RestartSec=1' "$ANYTLS_DEDUP_SYSTEMD_UNIT"
grep -Fq 'AF_UNIX' "$ANYTLS_SYSTEMD_UNIT"
grep -Fq "Wants=$ANYTLS_DEDUP_SERVICE_NAME.service" "$ANYTLS_SYSTEMD_UNIT"
if grep -Eq 'Requires=|BindsTo=|PartOf=' "$ANYTLS_SYSTEMD_UNIT"; then exit 1; fi
"$installer" doctor >"$test_root/dedup-doctor.log" 2>&1
"$installer" "${dedup_args[@]}" --without-dedup >/dev/null
[[ -f "$MOCK_SYSTEMCTL_ACTIVE" && ! -e "$MOCK_SYSTEMCTL_ACTIVE.dedup" && ! -e "$MOCK_SYSTEMCTL_ENABLED.dedup" ]]
grep -q '^ANYTLS_DEDUP_ENABLED=0$' "$ANYTLS_ENV_FILE"
grep -Eq "^ANYTLS_DEDUP_BACKEND=('')?$" "$ANYTLS_ENV_FILE"
# An ordinary reinstall preserves an explicit opt-out, including with an old binary.
"$installer" "${dedup_args[@]}" --binary "$old_binary" >/dev/null
[[ ! -e "$MOCK_SYSTEMCTL_ACTIVE.dedup" ]]
grep -q '^ANYTLS_DEDUP_ENABLED=0$' "$ANYTLS_ENV_FILE"
"$installer" "${dedup_args[@]}" --with-dedup >/dev/null
[[ -f "$MOCK_SYSTEMCTL_ACTIVE.dedup" ]]
# Menu switches reuse the transaction, preserve credentials and do not fetch.
sha256sum "$ANYTLS_PASSWORD_FILE" >"$test_root/menu-password.sha"
for enabled in 0 1; do
  ANYTLS_INSTALLER_LIB_ONLY=1 bash -c '
    installer=$1 enabled=$2
    set --
    source "$installer"
    with_deployment_lock set_dedup_enabled "$enabled"
  ' unused "$installer" "$enabled" >/dev/null
  grep -q "^ANYTLS_DEDUP_ENABLED=$enabled$" "$ANYTLS_ENV_FILE"
  sha256sum -c "$test_root/menu-password.sha" >/dev/null
done

snapshot_decoder_install() {
  sha256sum "$ANYTLS_BIN_PATH" "$ANYTLS_ENV_FILE" "$ANYTLS_PASSWORD_FILE" "$ANYTLS_SYSTEMD_UNIT" \
    "$ANYTLS_PROBE_BIN_PATH" "$ANYTLS_DEDUP_BIN_PATH" "$ANYTLS_DEDUP_SYSTEMD_UNIT" "$ANYTLS_CONFIG_DIR/dedup.env"
}
expect_decoder_failure() {
  snapshot_decoder_install >"$test_root/before.sha"
  if "$installer" "${dedup_args[@]}" "$@" >"$test_root/expected-failure.log" 2>&1; then
    echo "expected decoder deployment failure: $*" >&2
    exit 1
  fi
  snapshot_decoder_install >"$test_root/after.sha"
  cmp "$test_root/before.sha" "$test_root/after.sha"
  [[ -f "$MOCK_SYSTEMCTL_ACTIVE" && -f "$MOCK_SYSTEMCTL_ACTIVE.dedup" && -f "$MOCK_SYSTEMCTL_ENABLED.dedup" ]]
}
expect_decoder_failure --with-dedup --without-dedup
expect_decoder_failure --binary "$old_binary"
for injected in "$MOCK_DEDUP_FAIL_RESTART_ONCE" "$MOCK_DEDUP_HEALTH_FAIL" "$MOCK_DEDUP_PROBE_FAIL" "$MOCK_DEDUP_EPOCH_MISMATCH" "$MOCK_CP_FAIL"; do
  touch "$injected"
  expect_decoder_failure
  rm -f "$injected"
done
printf '%s' 'anytls-dedup.new' >"$MOCK_INSTALL_FAIL_MATCH_FILE"
expect_decoder_failure
rm -f "$MOCK_INSTALL_FAIL_MATCH_FILE"
touch "$MOCK_DEDUP_FAIL_STOP_ONCE"
expect_decoder_failure --without-dedup
# Restoring a main unit's Wants= must not change a stopped decoder's state.
for original_active in 0 1; do
  for original_enabled in 0 1; do
    rm -f "$MOCK_SYSTEMCTL_ACTIVE.dedup" "$MOCK_SYSTEMCTL_ENABLED.dedup"
    [[ "$original_active" == 0 ]] || touch "$MOCK_SYSTEMCTL_ACTIVE.dedup"
    [[ "$original_enabled" == 0 ]] || touch "$MOCK_SYSTEMCTL_ENABLED.dedup"
    snapshot_decoder_install >"$test_root/before.sha"
    touch "$MOCK_DEDUP_HEALTH_FAIL"
    if "$installer" "${dedup_args[@]}" >"$test_root/expected-failure.log" 2>&1; then exit 1; fi
    rm -f "$MOCK_DEDUP_HEALTH_FAIL"
    snapshot_decoder_install >"$test_root/after.sha"
    cmp "$test_root/before.sha" "$test_root/after.sha"
    if [[ "$original_active" == 1 ]]; then [[ -f "$MOCK_SYSTEMCTL_ACTIVE.dedup" ]]; else [[ ! -e "$MOCK_SYSTEMCTL_ACTIVE.dedup" ]]; fi
    if [[ "$original_enabled" == 1 ]]; then [[ -f "$MOCK_SYSTEMCTL_ENABLED.dedup" ]]; else [[ ! -e "$MOCK_SYSTEMCTL_ENABLED.dedup" ]]; fi
  done
done
touch "$MOCK_SYSTEMCTL_ACTIVE.dedup" "$MOCK_SYSTEMCTL_ENABLED.dedup"
printf '%s\n' 123 >"$ANYTLS_CGROUP_ROOT/mock/memory.max"
expect_decoder_failure
printf '%s\n' 524288000 >"$ANYTLS_CGROUP_ROOT/mock/memory.max"
# Missing state migrates to enabled, while a custom unmanaged backend is rejected.
sed -i '/^ANYTLS_DEDUP_ENABLED=/d; /^ANYTLS_DEDUP_BACKEND=/d' "$ANYTLS_ENV_FILE"
"$installer" "${dedup_args[@]}" >/dev/null
grep -q '^ANYTLS_DEDUP_ENABLED=1$' "$ANYTLS_ENV_FILE"
cp "$ANYTLS_CONFIG_DIR/dedup.env" "$test_root/good-dedup.env"
sed -i "s/^ANYTLS_DEDUP_DOMAINS=.*/ANYTLS_DEDUP_DOMAINS=''/" "$ANYTLS_CONFIG_DIR/dedup.env"
expect_decoder_failure
cp "$test_root/good-dedup.env" "$ANYTLS_CONFIG_DIR/dedup.env"
mkdir -p "$(dirname "$ANYTLS_DEDUP_RUNTIME_DIR")"
printf unsafe >"$ANYTLS_DEDUP_RUNTIME_DIR"
expect_decoder_failure
[[ "$(cat "$ANYTLS_DEDUP_RUNTIME_DIR")" == unsafe ]]
rm "$ANYTLS_DEDUP_RUNTIME_DIR"
"$installer" uninstall --non-interactive >/dev/null
[[ ! -e "$ANYTLS_DEDUP_BIN_PATH" && ! -e "$ANYTLS_DEDUP_SYSTEMD_UNIT" && ! -e "$ANYTLS_PROBE_BIN_PATH" ]]
[[ ! -e "$MOCK_SYSTEMCTL_ACTIVE.dedup" && ! -e "$MOCK_SYSTEMCTL_ENABLED.dedup" ]]
"$installer" uninstall --non-interactive >/dev/null
# A missing unit/config after an earlier cleanup does not prevent retry.
echo 'decoder default installation, opt-out, migration and rollback tests passed'

original_repo_root="$repo_root"
git_remote="$test_root/git-remote.git"
git_seed="$test_root/git-seed"
git_checkout="$test_root/git-checkout"
git init --bare -q "$git_remote"
git init -q "$git_seed"
git -C "$git_seed" config user.name test
git -C "$git_seed" config user.email test@example.com
printf '%s\n' initial >"$git_seed/tracked"
git -C "$git_seed" add tracked
git -C "$git_seed" commit -qm initial
git -C "$git_seed" branch -M zji-dev
git -C "$git_seed" remote add origin "$git_remote"
git -C "$git_seed" push -q -u origin zji-dev
git clone -q --branch zji-dev "$git_remote" "$git_checkout"
git -C "$git_checkout" config user.name test
git -C "$git_checkout" config user.email test@example.com
repo_root="$git_checkout"
# shellcheck disable=SC2034
expected_branch=zji-dev

printf '%s\n' untracked >"$git_checkout/untracked"
if update_source_tree >/dev/null 2>&1; then
  echo "update should reject untracked files" >&2
  exit 1
fi
rm -f "$git_checkout/untracked"
printf '%s\n' modified >"$git_checkout/tracked"
if update_source_tree >/dev/null 2>&1; then
  echo "update should reject tracked changes" >&2
  exit 1
fi
git -C "$git_checkout" restore tracked

printf '%s\n' remote >>"$git_seed/tracked"
git -C "$git_seed" commit -qam remote
git -C "$git_seed" push -q
update_source_tree >/dev/null
[[ "$(git -C "$git_checkout" rev-parse HEAD)" != "$(git -C "$git_checkout" rev-parse origin/zji-dev)" ]]
printf '%s\n' local >>"$git_checkout/tracked"
git -C "$git_checkout" commit -qam local
if update_source_tree >/dev/null 2>&1; then
  echo "update should reject diverged history" >&2
  exit 1
fi
repo_root="$original_repo_root"

echo "source update validation tests passed"

update_seed="$test_root/update-seed"
update_remote="$test_root/update-remote.git"
update_checkout="$test_root/update-checkout"
update_candidate="$test_root/update-candidate"
mkdir -p "$update_seed"
for source in AGENTS.md .gitignore cmd proxy util scripts docs go.mod go.sum readme.md; do
  cp -a "$original_repo_root/$source" "$update_seed/"
done
git -C "$update_seed" init -q
git -C "$update_seed" config user.name test
git -C "$update_seed" config user.email test@example.com
git -C "$update_seed" add -A
git -C "$update_seed" commit -qm baseline
git -C "$update_seed" branch -M zji-dev
baseline_commit="$(git -C "$update_seed" rev-parse HEAD)"
git init --bare -q "$update_remote"
git -C "$update_seed" remote add origin "$update_remote"
git -C "$update_seed" push -q -u origin zji-dev
git clone -q --branch zji-dev "$update_remote" "$update_checkout"
git clone -q --branch zji-dev "$update_remote" "$update_candidate"
git -C "$update_candidate" config user.name test
git -C "$update_candidate" config user.email test@example.com
printf '%s\n' candidate >"$update_candidate/candidate-marker"
git -C "$update_candidate" add candidate-marker
git -C "$update_candidate" commit -qm candidate
git -C "$update_candidate" push -q
candidate_commit="$(git -C "$update_candidate" rev-parse HEAD)"

update_installer="$update_checkout/scripts/install-anytls-server.sh"
"$update_installer" "${install_args[@]}" --binary "$old_binary" >/dev/null
export MOCK_PROBE_TOUCH_FILE="$update_checkout/update-interference"
if "$update_installer" update --non-interactive --no-firewall >/dev/null 2>&1; then
  echo "update should roll back when the source checkout changes during deployment" >&2
  exit 1
fi
unset MOCK_PROBE_TOUCH_FILE
rm -f "$update_checkout/update-interference"
[[ "$(git -C "$update_checkout" rev-parse HEAD)" == "$baseline_commit" ]]
cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
[[ -f "$MOCK_SYSTEMCTL_ACTIVE" ]]

export MOCK_GIT_INTERRUPT_ROOT="$update_checkout"
if "$update_installer" update --non-interactive --no-firewall >"$test_root/interrupted-update.log" 2>&1; then
  echo 'interruption after source advancement should roll back' >&2
  exit 1
fi
unset MOCK_GIT_INTERRUPT_ROOT
[[ "$(git -C "$update_checkout" rev-parse HEAD)" == "$baseline_commit" ]]
cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
"$update_installer" update --non-interactive --no-firewall >/dev/null
[[ "$(git -C "$update_checkout" rev-parse HEAD)" == "$candidate_commit" ]]
if cmp -s "$old_binary" "$ANYTLS_BIN_PATH"; then
  echo "successful update did not install the candidate binary" >&2
  exit 1
fi
"$update_installer" uninstall --non-interactive >/dev/null

echo "two-phase source/service update tests passed"

ANYTLS_BOOTSTRAP_LIB_ONLY=1 bash -c '
  source scripts/bootstrap-anytls-server.sh
  [[ "$(normalize_repo_url https://example.com/repo.git/)" == "https://example.com/repo" ]]
  install_dir=/opt/anytls-go
  repo_url=https://example.com/repo.git
  branch=zji-dev
  go_version=go1.26.3
  validate_inputs
'

echo "bootstrap validation tests passed"

# Check package selection and failure handling without invoking a host manager.
ANYTLS_BOOTSTRAP_LIB_ONLY=1 bash -s -- "$script_dir/bootstrap-anytls-server.sh" "$test_root" <<'EOF'
set -euo pipefail
bootstrap_script=$1 package_test_root=$2
set --
source "$bootstrap_script"
(
  command() {
    [[ "$*" != '-v cmp' ]] || return 1
    builtin command "$@"
  }
  if verify_bootstrap_dependencies >"$package_test_root/missing-tools.log" 2>&1; then
    echo 'dependency check accepted missing cmp' >&2
    exit 1
  fi
  grep -q 'missing required deployment tools.*cmp' "$package_test_root/missing-tools.log"
)
command() {
  case "${1:-}:${2:-}" in
    -v:apt-get|-v:dnf|-v:yum|-v:apk) [[ "$2" == "$test_manager" ]] ;;
    *) builtin command "$@" ;;
  esac
}
record_package_call() {
  printf '%s\n' "$*" >>"$package_test_root/packages.log"
  [[ "$fail_packages" == 0 ]]
}
apt-get() { record_package_call apt-get "$@"; }
dnf() { record_package_call dnf "$@"; }
yum() { record_package_call yum "$@"; }
apk() { record_package_call apk "$@"; }
verify_bootstrap_dependencies() { echo checked >>"$package_test_root/packages.log"; }
for test_manager in apt-get dnf yum apk; do
  fail_packages=0
  : >"$package_test_root/packages.log"
  install_packages
  grep -q 'coreutils diffutils findutils grep sed gawk' "$package_test_root/packages.log"
  grep -qx checked "$package_test_root/packages.log"
  case "$test_manager" in
    apt-get) grep -q 'passwd iproute2 systemd' "$package_test_root/packages.log" ;;
    dnf|yum) grep -q 'shadow-utils iproute systemd' "$package_test_root/packages.log" ;;
    apk) grep -q 'shadow iproute2' "$package_test_root/packages.log" ;;
  esac
  fail_packages=1
  : >"$package_test_root/packages.log"
  if install_packages; then echo 'package failure was ignored' >&2; exit 1; fi
  if grep -qx checked "$package_test_root/packages.log"; then exit 1; fi
done
EOF
echo 'bootstrap dependency installation and preflight tests passed'

ANYTLS_BOOTSTRAP_LIB_ONLY=1 bash -c '
  bootstrap_script=$1
  set -- menu --no-firewall
  source "$bootstrap_script"
  bootstrap_main() { printf "%s\n" "${installer_args[@]}"; }
  bootstrap_menu_action 7
' unused "$script_dir/bootstrap-anytls-server.sh" >"$test_root/menu-options.log"
grep -qx -- --no-firewall "$test_root/menu-options.log"
grep -qx -- --with-dedup "$test_root/menu-options.log"

# Prove a bootstrap upgrade never executes the obsolete local installer.
bootstrap_remote="$test_root/bootstrap-remote.git"
bootstrap_seed="$test_root/bootstrap-seed"
bootstrap_checkout="$test_root/bootstrap-checkout"
mkdir -p "$bootstrap_seed"
for source in AGENTS.md .gitignore cmd proxy util scripts docs go.mod go.sum readme.md; do
  cp -a "$original_repo_root/$source" "$bootstrap_seed/"
done
git -C "$bootstrap_seed" init -q
git -C "$bootstrap_seed" config user.name test
git -C "$bootstrap_seed" config user.email test@example.com
cp "$bootstrap_seed/scripts/install-anytls-server.sh" "$test_root/candidate-installer"
printf '#!/usr/bin/env bash\necho "obsolete installer was executed" >&2\nexit 66\n' >"$bootstrap_seed/scripts/install-anytls-server.sh"
git -C "$bootstrap_seed" add -A
git -C "$bootstrap_seed" commit -qm obsolete-installer
git -C "$bootstrap_seed" branch -M zji-dev
bootstrap_old_commit="$(git -C "$bootstrap_seed" rev-parse HEAD)"
git init --bare -q "$bootstrap_remote"
git -C "$bootstrap_seed" remote add origin "$bootstrap_remote"
git -C "$bootstrap_seed" push -q -u origin zji-dev
git clone -q --branch zji-dev "$bootstrap_remote" "$bootstrap_checkout"
cp "$test_root/candidate-installer" "$bootstrap_seed/scripts/install-anytls-server.sh"
git -C "$bootstrap_seed" add scripts/install-anytls-server.sh
git -C "$bootstrap_seed" commit -qm candidate-installer
git -C "$bootstrap_seed" push -q
bootstrap_new_commit="$(git -C "$bootstrap_seed" rev-parse HEAD)"
"$installer" "${install_args[@]}" --binary "$old_binary" >/dev/null
sed -i '/^ANYTLS_DEDUP_ENABLED=/d; /^ANYTLS_DEDUP_BACKEND=/d' "$ANYTLS_ENV_FILE"
cp "$ANYTLS_ENV_FILE" "$test_root/legacy.env"
# Use the latest downloaded entry, overriding only package/toolchain installation
# so the test never changes the host OS. Git, candidate handoff and transactions run.
run_bootstrap_fixture() {
  ANYTLS_REPO_URL="$bootstrap_remote" ANYTLS_SRC_DIR="$bootstrap_checkout" \
    ANYTLS_BOOTSTRAP_LIB_ONLY=1 bash -c '
      bootstrap_script=$1
      bootstrap_action=$2
      set -- "$bootstrap_action" --non-interactive --no-firewall
      source "$bootstrap_script"
      install_packages() { :; }
      ensure_go() { :; }
      # Exercise the downloaded menu dispatch without requiring a terminal.
      bootstrap_menu() { bootstrap_menu_action 2; }
      bootstrap_main
    ' unused "$script_dir/bootstrap-anytls-server.sh" "${1:-update}"
}
touch "$MOCK_DEDUP_PROBE_FAIL"
if run_bootstrap_fixture >"$test_root/bootstrap-failure.log" 2>&1; then
  echo 'legacy bootstrap upgrade should fail and roll back on decoder probe failure' >&2
  exit 1
fi
rm -f "$MOCK_DEDUP_PROBE_FAIL"
[[ "$(git -C "$bootstrap_checkout" rev-parse HEAD)" == "$bootstrap_old_commit" ]]
cmp "$test_root/legacy.env" "$ANYTLS_ENV_FILE"
cmp "$old_binary" "$ANYTLS_BIN_PATH"
[[ ! -e "$ANYTLS_DEDUP_BIN_PATH" && ! -e "$ANYTLS_DEDUP_SYSTEMD_UNIT" ]]
export MOCK_GIT_FAIL_FETCH_ROOT="$bootstrap_checkout"
if run_bootstrap_fixture menu >"$test_root/bootstrap-fetch-failure.log" 2>&1; then
  echo 'menu upgrade should fail before deployment when fetch fails' >&2
  exit 1
fi
unset MOCK_GIT_FAIL_FETCH_ROOT
grep -q 'injected fetch failure' "$test_root/bootstrap-fetch-failure.log"
[[ "$(git -C "$bootstrap_checkout" rev-parse HEAD)" == "$bootstrap_old_commit" ]]
cmp "$test_root/legacy.env" "$ANYTLS_ENV_FILE"
cmp "$old_binary" "$ANYTLS_BIN_PATH"
[[ ! -e "$ANYTLS_DEDUP_BIN_PATH" && ! -e "$ANYTLS_DEDUP_SYSTEMD_UNIT" ]]
run_bootstrap_fixture menu >"$test_root/bootstrap-success.log" 2>&1
[[ "$(git -C "$bootstrap_checkout" rev-parse HEAD)" == "$bootstrap_new_commit" ]]
grep -q '^ANYTLS_DEDUP_ENABLED=1$' "$ANYTLS_ENV_FILE"
[[ -f "$MOCK_SYSTEMCTL_ACTIVE.dedup" && -f "$MOCK_SYSTEMCTL_ACTIVE" ]]
[[ "$(git -C "$bootstrap_checkout" worktree list --porcelain | grep -c '^worktree ')" == 1 ]]
"$installer" uninstall --non-interactive >/dev/null
echo 'legacy bootstrap menu/update decoder migration and rollback tests passed'

# Both omitted and explicit menu actions stay in the downloaded entry. Merely
# opening the menu must neither fetch source nor execute an obsolete installer.
for menu_action in default menu; do
  ANYTLS_REPO_URL="$bootstrap_remote" ANYTLS_SRC_DIR="$bootstrap_checkout" \
    ANYTLS_BOOTSTRAP_LIB_ONLY=1 bash -c '
      bootstrap_script=$1 menu_action=$2
      set --
      [[ "$menu_action" == default ]] || set -- menu
      source "$bootstrap_script"
      install_packages() { :; }
      ensure_go() { echo "menu unexpectedly installed Go" >&2; exit 1; }
      bootstrap_menu() { echo downloaded-menu; }
      bootstrap_main
    ' unused "$script_dir/bootstrap-anytls-server.sh" "$menu_action" >"$test_root/menu.log"
  grep -qx downloaded-menu "$test_root/menu.log"
done
echo 'bootstrap default and explicit menu entry tests passed'
