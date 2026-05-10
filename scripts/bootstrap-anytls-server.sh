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
  ANYTLS_GO_VERSION   Go version to install if missing. Default: go1.26.3

Arguments after "--" are passed to install-anytls-server.sh.
USAGE
}

installer_args=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --)
      shift
      installer_args=("$@")
      break
      ;;
    --repo)
      repo_url="${2:-}"
      shift 2
      ;;
    --branch)
      branch="${2:-}"
      shift 2
      ;;
    --dir)
      install_dir="${2:-}"
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

if [[ "$(id -u)" -ne 0 ]]; then
  echo "please run as root, for example: sudo bash bootstrap-anytls-server.sh" >&2
  exit 1
fi

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac
}

install_packages() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl git tar gzip
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ca-certificates curl git tar gzip
  elif command -v yum >/dev/null 2>&1; then
    yum install -y ca-certificates curl git tar gzip
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache ca-certificates curl git tar gzip
  else
    echo "unsupported package manager; please install ca-certificates curl git tar gzip manually" >&2
    exit 1
  fi
}

ensure_go() {
  if command -v go >/dev/null 2>&1; then
    echo "found $(go version)"
    return
  fi

  arch="$(detect_arch)"
  tarball="${go_version}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' EXIT

  echo "installing ${go_version} for linux/${arch}..."
  curl -fL --retry 3 -o "$tmp_dir/$tarball" "$url"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmp_dir/$tarball"
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

install_packages
ensure_go

if [[ -d "$install_dir/.git" ]]; then
  echo "updating source tree: $install_dir"
  git -C "$install_dir" fetch origin "$branch"
  git -C "$install_dir" checkout "$branch"
  git -C "$install_dir" pull --ff-only origin "$branch"
else
  echo "cloning $repo_url branch $branch to $install_dir"
  rm -rf "$install_dir"
  git clone --branch "$branch" --single-branch "$repo_url" "$install_dir"
fi

echo "pre-downloading Go modules..."
(
  cd "$install_dir"
  go mod download
)

echo
echo "starting installer..."
exec "$install_dir/scripts/install-anytls-server.sh" "${installer_args[@]}" --branch "$branch"
