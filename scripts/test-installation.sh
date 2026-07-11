#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
installer="$script_dir/install-anytls-server.sh"
test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT

ANYTLS_INSTALLER_LIB_ONLY=1 source "$installer"
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
exec "$REAL_INSTALL" "${args[@]}"
EOF

cat >"$mock_bin/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$MOCK_SYSTEMCTL_LOG"
case "${1:-}" in
  is-active|is-enabled) exit 0 ;;
  restart)
    [[ ! -f "$MOCK_SYSTEMCTL_FAIL_RESTART" ]]
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
chmod +x "$mock_bin/id" "$mock_bin/install" "$mock_bin/systemctl" "$mock_bin/ss" "$mock_bin/journalctl"

old_binary="$test_root/old-server"
new_binary="$test_root/new-server"
printf '#!/usr/bin/env bash\necho old\n' >"$old_binary"
printf '#!/usr/bin/env bash\necho new\n' >"$new_binary"
chmod +x "$old_binary" "$new_binary"

export PATH="$mock_bin:$PATH"
export REAL_ID="$real_id"
export REAL_INSTALL="$real_install"
export MOCK_SYSTEMCTL_LOG="$test_root/systemctl.log"
export MOCK_SYSTEMCTL_FAIL_RESTART="$test_root/fail-restart"
export ANYTLS_SERVICE_NAME="anytls-test"
export ANYTLS_SERVICE_USER="root"
export ANYTLS_SERVICE_GROUP="root"
export ANYTLS_CONFIG_DIR="$test_root/etc/anytls"
export ANYTLS_ENV_FILE="$ANYTLS_CONFIG_DIR/server.env"
export ANYTLS_PASSWORD_FILE="$ANYTLS_CONFIG_DIR/server.password"
export ANYTLS_FIREWALL_STATE_FILE="$ANYTLS_CONFIG_DIR/firewall.state"
export ANYTLS_BIN_PATH="$test_root/usr/local/bin/anytls-server"
export ANYTLS_SYSTEMD_UNIT="$test_root/etc/systemd/system/anytls-test.service"
mkdir -p "$(dirname "$ANYTLS_BIN_PATH")" "$(dirname "$ANYTLS_SYSTEMD_UNIT")"

install_args=(
  install --non-interactive --no-firewall
  --password 'secret /@value'
  --listen '0.0.0.0:8443'
  --server-name '2001:db8::1'
  --fallback ''
)

"$installer" "${install_args[@]}" --binary "$old_binary" >/dev/null

cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
[[ "$(<"$ANYTLS_PASSWORD_FILE")" == 'secret /@value' ]]
! grep -Fq 'secret /@value' "$ANYTLS_ENV_FILE"
! grep -Fq 'secret /@value' "$ANYTLS_SYSTEMD_UNIT"
grep -Fq "EnvironmentFile=$ANYTLS_ENV_FILE" "$ANYTLS_SYSTEMD_UNIT"
grep -Fq "ExecStart=$ANYTLS_BIN_PATH" "$ANYTLS_SYSTEMD_UNIT"
grep -Fq 'User=root' "$ANYTLS_SYSTEMD_UNIT"

"$installer" doctor >/dev/null

touch "$MOCK_SYSTEMCTL_FAIL_RESTART"
if "$installer" "${install_args[@]}" --binary "$new_binary" >/dev/null 2>&1; then
  echo "expected failed restart to fail installation" >&2
  exit 1
fi
cmp -s "$old_binary" "$ANYTLS_BIN_PATH"
rm -f "$MOCK_SYSTEMCTL_FAIL_RESTART"

"$installer" uninstall --non-interactive >/dev/null
[[ ! -e "$ANYTLS_BIN_PATH" ]]
[[ ! -e "$ANYTLS_CONFIG_DIR" ]]
[[ ! -e "$ANYTLS_SYSTEMD_UNIT" ]]

echo "installation flow tests passed"

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
