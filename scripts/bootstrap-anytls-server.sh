#!/usr/bin/env bash
set -euo pipefail

repo_url="${ANYTLS_REPO_URL:-https://github.com/zji996/anytls-go.git}"
branch="${ANYTLS_BRANCH:-zji-dev}"
install_dir="${ANYTLS_SRC_DIR:-/opt/anytls-go}"
go_version="${ANYTLS_GO_VERSION:-go1.26.3}"

usage() {
  cat <<'USAGE'
Usage:
  bootstrap-anytls-server.sh [--repo URL] [--branch zji-dev] [--dir /opt/anytls-go] [-- INSTALLER_ARGS...]
  bootstrap-anytls-server.sh install --non-interactive

Environment:
  ANYTLS_REPO_URL     Repository URL. Default: https://github.com/zji996/anytls-go.git
  ANYTLS_BRANCH       Branch to deploy. Default: zji-dev
  ANYTLS_SRC_DIR      Source checkout directory. Default: /opt/anytls-go
  ANYTLS_GO_VERSION   Go version to install if Go is older than go.mod requires. Default: go1.26.3

Arguments after "--" are passed to install-anytls-server.sh.
USAGE
}

installer_args=()

require_option_value() {
  if [[ $# -lt 2 || -z "$2" ]]; then
    echo "option $1 requires a value" >&2
    exit 2
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --)
      shift
      installer_args=("$@")
      break
      ;;
    --repo)
      require_option_value "$@"
      repo_url="$2"
      shift 2
      ;;
    --branch)
      require_option_value "$@"
      branch="$2"
      shift 2
      ;;
    --dir)
      require_option_value "$@"
      install_dir="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    install|update|status|doctor|restart|uninstall|menu)
      installer_args=("$@")
      break
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac
}

validate_install_dir() {
  if [[ "$install_dir" != /* || "$install_dir" == "/" || "$install_dir" == "/opt" || "$install_dir" == "/usr" || "$install_dir" == "/root" || "$install_dir" == "$HOME" ]]; then
    echo "unsafe source directory: '$install_dir'; choose a dedicated absolute path" >&2
    exit 1
  fi
}

validate_inputs() {
  validate_install_dir
  if [[ -z "$repo_url" || "$repo_url" == -* || "$repo_url" == *$'\n'* ]]; then
    echo "invalid repository URL/path: '$repo_url'" >&2
    exit 1
  fi
  if [[ ! "$go_version" =~ ^go[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]; then
    echo "invalid Go version: '$go_version'" >&2
    exit 1
  fi
  if ! git check-ref-format --branch "$branch" >/dev/null 2>&1; then
    echo "invalid branch name: '$branch'" >&2
    exit 1
  fi
}

normalize_repo_url() {
  local value="$1"
  value="${value%/}"
  value="${value%.git}"
  printf '%s' "$value"
}

install_packages() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl git tar gzip python3 util-linux
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ca-certificates curl git tar gzip python3 util-linux
  elif command -v yum >/dev/null 2>&1; then
    yum install -y ca-certificates curl git tar gzip python3 util-linux
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache ca-certificates curl git tar gzip python3 util-linux
  else
    echo "unsupported package manager; please install ca-certificates curl git tar gzip python3 util-linux manually" >&2
    exit 1
  fi
}

ensure_go() {
  local required_version installed_version
  required_version="${1:-}"
  if [[ -z "$required_version" ]]; then
    required_version="$(awk '/^go / { print $2; exit }' "$install_dir/go.mod" 2>/dev/null || true)"
  fi
  required_version="${required_version:-1.24.0}"
  if command -v go >/dev/null 2>&1; then
    installed_version="$(go env GOVERSION 2>/dev/null | sed 's/^go//' || true)"
    if [[ -n "$installed_version" && "$(printf '%s\n%s\n' "$required_version" "$installed_version" | sort -V | head -n 1)" == "$required_version" ]]; then
      echo "found $(go version), satisfies required Go $required_version"
      return
    fi
    echo "installed Go ${installed_version:-unknown} is older than required Go $required_version"
  fi
  local requested_version="${go_version#go}"
  if [[ "$(printf '%s\n%s\n' "$required_version" "$requested_version" | sort -V | head -n 1)" != "$required_version" ]]; then
    echo "requested $go_version is older than required Go $required_version" >&2
    exit 1
  fi

  local arch tarball url tmp_dir expected_checksum actual_checksum backup_dir
  local go_link_existed=0 gofmt_link_existed=0
  arch="$(detect_arch)"
  tarball="${go_version}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"
  tmp_dir="$(mktemp -d)"
  backup_dir=""
  trap 'rm -rf "${tmp_dir:-}"' EXIT

  echo "installing ${go_version} for linux/${arch}..."
  curl -fL --retry 3 -o "$tmp_dir/$tarball" "$url"
  curl -fsSL --retry 3 -o "$tmp_dir/downloads.json" 'https://go.dev/dl/?mode=json&include=all'
  expected_checksum="$(python3 - "$tmp_dir/downloads.json" "$tarball" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    releases = json.load(source)
for release in releases:
    for download in release.get("files", []):
        if download.get("filename") == sys.argv[2]:
            print(download.get("sha256", ""))
            raise SystemExit
raise SystemExit(1)
PY
)"
  actual_checksum="$(sha256sum "$tmp_dir/$tarball" | awk '{print $1}')"
  if [[ ! "$expected_checksum" =~ ^[0-9a-fA-F]{64}$ || "$actual_checksum" != "$expected_checksum" ]]; then
    rm -rf "$tmp_dir"
    echo "Go archive SHA256 verification failed" >&2
    exit 1
  fi
  tar -C "$tmp_dir" -xzf "$tmp_dir/$tarball"
  if [[ -e /usr/local/bin/go || -L /usr/local/bin/go ]]; then
    cp -a /usr/local/bin/go "$tmp_dir/go.link.backup"
    go_link_existed=1
  fi
  if [[ -e /usr/local/bin/gofmt || -L /usr/local/bin/gofmt ]]; then
    cp -a /usr/local/bin/gofmt "$tmp_dir/gofmt.link.backup"
    gofmt_link_existed=1
  fi
  if [[ -d /usr/local/go ]]; then
    backup_dir="/usr/local/go.backup.$$"
    mv /usr/local/go "$backup_dir"
  fi
  if ! mv "$tmp_dir/go" /usr/local/go; then
    [[ -n "$backup_dir" ]] && mv "$backup_dir" /usr/local/go
    rm -rf "$tmp_dir"
    exit 1
  fi
  if ! /usr/local/go/bin/go version >/dev/null 2>&1 || ! ln -sf /usr/local/go/bin/go /usr/local/bin/go || ! ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt; then
    rm -rf /usr/local/go
    [[ -n "$backup_dir" ]] && mv "$backup_dir" /usr/local/go
    rm -f /usr/local/bin/go /usr/local/bin/gofmt
    [[ "$go_link_existed" -eq 1 ]] && cp -a "$tmp_dir/go.link.backup" /usr/local/bin/go
    [[ "$gofmt_link_existed" -eq 1 ]] && cp -a "$tmp_dir/gofmt.link.backup" /usr/local/bin/gofmt
    echo "new Go toolchain failed validation; restored previous installation" >&2
    exit 1
  fi
  rm -rf "$backup_dir" "$tmp_dir"
  trap - EXIT
}

# Keep this menu in the downloaded bootstrap: an old checkout's menu must
# never coordinate an install/update of a newer staging format.
# shellcheck disable=SC2030
bootstrap_menu_action() (
  case "$1" in
    1) installer_args=(install "${installer_args[@]:1}") ;;
    2) installer_args=(update "${installer_args[@]:1}") ;;
    3) installer_args=(status) ;;
    4) installer_args=(doctor) ;;
    5) installer_args=(restart) ;;
    6) installer_args=(uninstall) ;;
    7) installer_args=(update "${installer_args[@]:1}" --with-dedup --non-interactive) ;;
    8) installer_args=(update "${installer_args[@]:1}" --without-dedup --non-interactive) ;;
    *) echo 'invalid choice' >&2; return 2 ;;
  esac
  # The parent menu catches failures with ||. A fresh Bash keeps errexit
  # effective inside package/toolchain setup, fetch and worktree creation.
  export repo_url branch install_dir go_version
  export -f detect_arch validate_install_dir validate_inputs normalize_repo_url \
    install_packages ensure_go bootstrap_main bootstrap_menu bootstrap_menu_action
  bash -euo pipefail -c 'installer_args=("$@"); bootstrap_main' anytls-bootstrap "${installer_args[@]}"
)

bootstrap_menu() {
  local choice
  while true; do
    echo
    echo 'AnyTLS Server Manager'
    echo '1) Install / Reinstall'
    echo "2) Update $branch and restart"
    echo '3) Status and client URI'
    echo '4) Doctor / self-check'
    echo '5) Restart'
    echo '6) Uninstall'
    echo '7) Enable request deduplication decoder'
    echo '8) Disable request deduplication decoder'
    echo '0) Exit'
    read -r -p 'Select [1]: ' choice </dev/tty || return 1
    choice="${choice:-1}"
    [[ "$choice" != 0 ]] || return 0
    bootstrap_menu_action "$choice" || echo 'action failed; see the error above' >&2
    # Uninstall can also remove the managed checkout.
    [[ -d "$install_dir/.git" ]] || return 0
  done
}

bootstrap_main() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "please run as root, for example: sudo bash bootstrap-anytls-server.sh" >&2
    exit 1
  fi

  validate_install_dir
  install_packages
  validate_inputs

  # The menu passes its subshell-local array to a fresh Bash as positional args.
  # shellcheck disable=SC2031
  local existing_checkout=0 requested_action="${installer_args[0]:-menu}" required_go=""
  # Hold the same lock through fetch, candidate deployment, source commit and cleanup.
  local bootstrap_lock_fd
  if [[ "$requested_action" == install || "$requested_action" == update ]]; then
    local lock_file="${ANYTLS_DEPLOYMENT_LOCK_FILE:-/run/lock/${ANYTLS_SERVICE_NAME:-anytls-server}.install.lock}"
    install -d -m 0755 "$(dirname "$lock_file")"
    exec {bootstrap_lock_fd}>"$lock_file"
    flock -n "$bootstrap_lock_fd" || { echo 'another AnyTLS deployment is already running' >&2; return 75; }
    export ANYTLS_DEPLOYMENT_LOCK_HELD=1
  fi
  if [[ -d "$install_dir/.git" ]]; then
    existing_checkout=1
    local existing_repo_url current_branch
    existing_repo_url="$(git -C "$install_dir" remote get-url origin 2>/dev/null || true)"
    if [[ "$(normalize_repo_url "$existing_repo_url")" != "$(normalize_repo_url "$repo_url")" ]]; then
      echo "refusing to update $install_dir: origin is '$existing_repo_url', expected '$repo_url'" >&2
      exit 1
    fi
    current_branch="$(git -C "$install_dir" branch --show-current)"
    if [[ "$requested_action" == "install" || "$requested_action" == "update" ]]; then
      if [[ -n "$(git -C "$install_dir" status --porcelain)" ]]; then
        echo "refusing to update $install_dir: working tree has local changes or untracked files" >&2
        exit 1
      fi
      if [[ "$current_branch" != "$branch" ]]; then
        echo "refusing to switch branch automatically: current '$current_branch', requested '$branch'" >&2
        exit 1
      fi
      echo "fetching candidate source: $install_dir"
      git -C "$install_dir" fetch origin "$branch:refs/remotes/origin/$branch"
      required_go="$(git -C "$install_dir" show "origin/$branch:go.mod" | awk '/^go / { print $2; exit }')"
    fi
  else
    local parent_dir install_name clone_dir
    if [[ -e "$install_dir" ]] && [[ -n "$(find "$install_dir" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]]; then
      echo "refusing to overwrite non-Git directory: $install_dir" >&2
      exit 1
    fi
    echo "cloning $repo_url branch $branch to $install_dir"
    parent_dir="$(dirname "$install_dir")"
    install_name="$(basename "$install_dir")"
    mkdir -p "$parent_dir"
    clone_dir="$(mktemp -d "$parent_dir/.${install_name}.clone.XXXXXX")"
    rmdir "$clone_dir"
    if ! git clone --branch "$branch" --single-branch "$repo_url" "$clone_dir"; then
      rm -rf "$clone_dir"
      exit 1
    fi
    rmdir "$install_dir" 2>/dev/null || true
    mv "$clone_dir" "$install_dir"
  fi

  touch "$install_dir/.anytls-bootstrap-managed"
  grep -qxF '/.anytls-bootstrap-managed' "$install_dir/.git/info/exclude" 2>/dev/null || printf '%s\n' '/.anytls-bootstrap-managed' >>"$install_dir/.git/info/exclude"

  if [[ "$existing_checkout" -eq 0 || "$requested_action" == "install" || "$requested_action" == "update" ]]; then
    ensure_go "$required_go"
  fi

  if [[ "$existing_checkout" -eq 0 ]]; then
    echo "pre-downloading Go modules..."
    (
      cd "$install_dir"
      go mod download
    )
  elif [[ "$requested_action" == "install" ]]; then
    installer_args[0]="update"
  fi

  if [[ "$existing_checkout" == 1 && ( "$requested_action" == install || "$requested_action" == update ) ]]; then
    local candidate_commit deploy_status=0
    candidate_commit="$(git -C "$install_dir" rev-parse "origin/$branch")"
    bootstrap_candidate_worktree="$(mktemp -d)"
    rmdir "$bootstrap_candidate_worktree"
    git -C "$install_dir" worktree add --detach "$bootstrap_candidate_worktree" "$candidate_commit"
    trap 'git -C "$install_dir" worktree remove --force "$bootstrap_candidate_worktree" >/dev/null 2>&1 || true' EXIT
    echo 'starting candidate-version installer (source checkout commits only after health checks)...'
    ANYTLS_UPDATE_REPO_ROOT="$install_dir" ANYTLS_UPDATE_COMMIT="$candidate_commit" \
      bash "$bootstrap_candidate_worktree/scripts/install-anytls-server.sh" "${installer_args[@]}" --branch "$branch" || deploy_status=$?
    git -C "$install_dir" worktree remove --force "$bootstrap_candidate_worktree" >/dev/null 2>&1 || true
    trap - EXIT
    return "$deploy_status"
  fi

  if [[ "$existing_checkout" == 1 && "$requested_action" == menu ]]; then
    bootstrap_menu
    return
  fi

  echo
  echo "starting installer..."
  exec "$install_dir/scripts/install-anytls-server.sh" "${installer_args[@]}" --branch "$branch"
}

if [[ "${ANYTLS_BOOTSTRAP_LIB_ONLY:-0}" != "1" ]]; then
  bootstrap_main
fi
