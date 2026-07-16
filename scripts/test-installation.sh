#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
installer="$script_dir/install-anytls-server.sh"
test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT

# shellcheck disable=SC1090
ANYTLS_INSTALLER_LIB_ONLY=1 source "$installer"
# shellcheck disable=SC2317
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

cat >"$mock_bin/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$MOCK_SYSTEMCTL_LOG"
case "${1:-}" in
  is-active) [[ -f "$MOCK_SYSTEMCTL_ACTIVE" ]] ;;
  is-enabled) [[ -f "$MOCK_SYSTEMCTL_ENABLED" ]] ;;
  show) echo 4242 ;;
  enable) touch "$MOCK_SYSTEMCTL_ENABLED" ;;
  disable)
    rm -f "$MOCK_SYSTEMCTL_ENABLED"
    [[ "$*" == *"--now"* ]] && rm -f "$MOCK_SYSTEMCTL_ACTIVE"
    ;;
  restart)
    if [[ -f "$MOCK_SYSTEMCTL_FAIL_RESTART_ONCE" ]]; then
      rm -f "$MOCK_SYSTEMCTL_FAIL_RESTART_ONCE"
      exit 1
    fi
    [[ ! -f "$MOCK_SYSTEMCTL_FAIL_RESTART" ]] || exit 1
    touch "$MOCK_SYSTEMCTL_ACTIVE"
    ;;
  stop) rm -f "$MOCK_SYSTEMCTL_ACTIVE" ;;
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
chmod +x "$mock_bin/id" "$mock_bin/install" "$mock_bin/go" "$mock_bin/cp" "$mock_bin/systemctl" "$mock_bin/ss" "$mock_bin/journalctl" "$mock_bin/sleep" "$mock_bin/probe" "$mock_bin/iptables"

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
export ANYTLS_SYSTEMD_UNIT="$test_root/etc/systemd/system/anytls-test.service"
export ANYTLS_DEPLOYMENT_LOCK_FILE="$test_root/anytls-test.install.lock"
mkdir -p "$(dirname "$ANYTLS_BIN_PATH")" "$(dirname "$ANYTLS_SYSTEMD_UNIT")"
mkdir -p "$MOCK_FIREWALL_RULE_DIR"

install_args=(
  install --non-interactive --no-firewall
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
cp -a "$original_repo_root/." "$update_seed/"
rm -rf "$update_seed/.git"
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
