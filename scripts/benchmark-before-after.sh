#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
baseline_ref=${1:-b9a46ac8fa1a0df6f7aa871f1d92ec6ee7e5e858}
baseline_dir=$(mktemp -d "${TMPDIR:-/tmp}/anytls-baseline.XXXXXX")

cleanup() {
  git -C "$repo_root" worktree remove --force "$baseline_dir" >/dev/null 2>&1 || true
}
trap cleanup EXIT

git -C "$repo_root" worktree add --detach "$baseline_dir" "$baseline_ref" >/dev/null
cp "$repo_root/proxy/session/performance_comparison_benchmark_test.go" "$baseline_dir/proxy/session/"
cp "$repo_root/util/mkcert_test.go" "$baseline_dir/util/performance_comparison_benchmark_test.go"

benchmark_pattern='Benchmark(StreamWritePersistentPipe|StreamReadQueued|SessionReceiveFrame)$'

echo "== baseline: $baseline_ref =="
(
  cd "$baseline_dir"
  go test -run '^$' -bench "$benchmark_pattern" -benchmem -count=5 ./proxy/session
  go test -run '^$' -bench BenchmarkPaddingSizesRandom -benchmem -count=5 ./proxy/session
  go test -run '^$' -bench BenchmarkTLSHandshake -benchmem -count=5 ./util
)

echo "== current worktree =="
(
  cd "$repo_root"
  go test -run '^$' -bench "$benchmark_pattern" -benchmem -count=5 ./proxy/session
  go test -run '^$' -bench BenchmarkPaddingSizesRandomSessionRNG -benchmem -count=5 ./proxy/session
  go test -run '^$' -bench BenchmarkTLSHandshake -benchmem -count=5 ./util
)
