#!/usr/bin/env bash
set -euo pipefail

anytls_proxy="socks5h://127.0.0.1:1080"
vision_proxy="socks5h://127.0.0.1:1081"
latency_url="https://www.cloudflare.com/cdn-cgi/trace"
download_url="https://speed.cloudflare.com/__down?bytes=52428800"
runs=5
warmup=1
connect_timeout=10
max_time=120
out_file=""

usage() {
  cat <<'USAGE'
Usage:
  compare-proxies.sh [options]

Compare two local proxy entries, intended for AnyTLS vs Xray/VLESS Vision on the
same remote server. Run this on the client side, not on the VPS.

Options:
      --anytls PROXY       AnyTLS local proxy URL. Default: socks5h://127.0.0.1:1080
      --vision PROXY       Vision local proxy URL. Default: socks5h://127.0.0.1:1081
      --latency-url URL    Small object URL for latency/TTFB. Default: Cloudflare trace
      --download-url URL   Large object URL for throughput. Default: 50 MiB Cloudflare speed
      --runs N             Measured runs per proxy/test. Default: 5
      --warmup N           Warmup runs per proxy/test, not counted. Default: 1
      --connect-timeout S  curl connect timeout. Default: 10
      --max-time S         curl max time per request. Default: 120
      --out FILE           Write TSV result to FILE. Default: ./proxy-compare-YYYYmmdd-HHMMSS.tsv
  -h, --help               Show help.

Examples:
  scripts/compare-proxies.sh
  scripts/compare-proxies.sh --anytls socks5h://127.0.0.1:1080 --vision socks5h://127.0.0.1:1081 --runs 10
  scripts/compare-proxies.sh --download-url https://speed.cloudflare.com/__down?bytes=104857600
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --anytls)
      anytls_proxy="${2:-}"
      shift 2
      ;;
    --vision)
      vision_proxy="${2:-}"
      shift 2
      ;;
    --latency-url)
      latency_url="${2:-}"
      shift 2
      ;;
    --download-url)
      download_url="${2:-}"
      shift 2
      ;;
    --runs)
      runs="${2:-}"
      shift 2
      ;;
    --warmup)
      warmup="${2:-}"
      shift 2
      ;;
    --connect-timeout)
      connect_timeout="${2:-}"
      shift 2
      ;;
    --max-time)
      max_time="${2:-}"
      shift 2
      ;;
    --out)
      out_file="${2:-}"
      shift 2
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

require_number() {
  local name="$1"
  local value="$2"
  if ! [[ "$value" =~ ^[0-9]+$ ]]; then
    echo "$name must be an integer: $value" >&2
    exit 2
  fi
}

require_number "--runs" "$runs"
require_number "--warmup" "$warmup"
require_number "--connect-timeout" "$connect_timeout"
require_number "--max-time" "$max_time"

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required" >&2
  exit 1
fi

if [[ -z "$out_file" ]]; then
  out_file="proxy-compare-$(date +%Y%m%d-%H%M%S).tsv"
fi

tmp_file="$(mktemp)"
trap 'rm -f "$tmp_file"' EXIT

printf "timestamp\tnode\ttest\trun\texit_code\thttp_code\ttime_connect_s\ttime_starttransfer_s\ttime_total_s\tsize_download_bytes\tspeed_download_Bps\turl\n" >"$tmp_file"

run_one() {
  local node="$1"
  local proxy="$2"
  local test_name="$3"
  local url="$4"
  local run_id="$5"
  local output exit_code

  set +e
  output="$(
    curl -L -o /dev/null -sS \
      --proxy "$proxy" \
      --connect-timeout "$connect_timeout" \
      --max-time "$max_time" \
      --write-out "%{http_code}\t%{time_connect}\t%{time_starttransfer}\t%{time_total}\t%{size_download}\t%{speed_download}" \
      "$url" 2>/dev/null
  )"
  exit_code=$?
  set -e

  if [[ -z "$output" ]]; then
    output="000\t0\t0\t0\t0\t0"
  fi

  printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\n" \
    "$(date -Is)" "$node" "$test_name" "$run_id" "$exit_code" "$output" "$url" >>"$tmp_file"
}

run_group() {
  local node="$1"
  local proxy="$2"
  local test_name="$3"
  local url="$4"
  local i

  for ((i = 1; i <= warmup; i++)); do
    printf "warmup %-6s %-8s %d/%d\n" "$node" "$test_name" "$i" "$warmup" >&2
    run_one "$node" "$proxy" "$test_name" "$url" "warmup-$i"
  done

  for ((i = 1; i <= runs; i++)); do
    printf "run    %-6s %-8s %d/%d\n" "$node" "$test_name" "$i" "$runs" >&2
    run_one "$node" "$proxy" "$test_name" "$url" "$i"
  done
}

echo "AnyTLS proxy: $anytls_proxy"
echo "Vision proxy: $vision_proxy"
echo "Latency URL: $latency_url"
echo "Download URL: $download_url"
echo

run_group "anytls" "$anytls_proxy" "latency" "$latency_url"
run_group "vision" "$vision_proxy" "latency" "$latency_url"
run_group "anytls" "$anytls_proxy" "download" "$download_url"
run_group "vision" "$vision_proxy" "download" "$download_url"

grep -v $'\twarmup-' "$tmp_file" >"$out_file"

echo
echo "Raw result: $out_file"
echo

awk -F '\t' '
NR == 1 { next }
$4 ~ /^warmup-/ { next }
{
  key = $2 SUBSEP $3
  total[key]++
  if ($5 == 0 && $6 >= 200 && $6 < 400) {
    ok[key]++
    connect[key] += $7
    ttfb[key] += $8
    elapsed[key] += $9
    bytes[key] += $10
    speed[key] += $11
  } else {
    fail[key]++
  }
}
END {
  printf "%-8s %-9s %5s %5s %12s %12s %12s %14s\n", "node", "test", "ok", "fail", "avg_ttfb_s", "avg_total_s", "avg_mbps", "download_MB"
  for (node_i = 1; node_i <= 2; node_i++) {
    node = node_i == 1 ? "anytls" : "vision"
    for (test_i = 1; test_i <= 2; test_i++) {
      test = test_i == 1 ? "latency" : "download"
      key = node SUBSEP test
      if (ok[key] > 0) {
        avg_ttfb = ttfb[key] / ok[key]
        avg_total = elapsed[key] / ok[key]
        avg_mbps = speed[key] * 8 / ok[key] / 1000000
        total_mb = bytes[key] / 1000000
      } else {
        avg_ttfb = 0
        avg_total = 0
        avg_mbps = 0
        total_mb = 0
      }
      printf "%-8s %-9s %5d %5d %12.4f %12.4f %12.2f %14.2f\n", node, test, ok[key] + 0, fail[key] + 0, avg_ttfb, avg_total, avg_mbps, total_mb
    }
  }

  a = "anytls" SUBSEP "download"
  v = "vision" SUBSEP "download"
  if (ok[a] > 0 && ok[v] > 0 && speed[v] > 0) {
    anytls_mbps = speed[a] * 8 / ok[a] / 1000000
    vision_mbps = speed[v] * 8 / ok[v] / 1000000
    printf "\nDownload throughput: AnyTLS %.2f Mbps vs Vision %.2f Mbps, delta %.1f%%\n", anytls_mbps, vision_mbps, (anytls_mbps / vision_mbps - 1) * 100
  }

  a = "anytls" SUBSEP "latency"
  v = "vision" SUBSEP "latency"
  if (ok[a] > 0 && ok[v] > 0 && ttfb[v] > 0) {
    anytls_ttfb = ttfb[a] / ok[a]
    vision_ttfb = ttfb[v] / ok[v]
    printf "Latency TTFB: AnyTLS %.4fs vs Vision %.4fs, delta %.1f%%\n", anytls_ttfb, vision_ttfb, (vision_ttfb - anytls_ttfb) / vision_ttfb * 100
  }
}
' "$tmp_file"
