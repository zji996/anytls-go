# 本地测试

本仓库的测试尽量设计成本机可运行，不依赖外部服务器、真实 TLS 证书或公网连通性。

当前分支最近一次完整验证项见 [zji-dev 当前状态](./status.md)。

## 基础检查

```
go test ./...
go vet ./...
```

## Session 本地集成测试

`proxy/session` 的集成测试使用 `net.Pipe` 模拟一条本地连接，覆盖 `Session.Run`、`OpenStream`、`Stream.Read`、`Stream.Write`、`cmdSYNACK` 错误传播和 Client 级 PaddingScheme 更新。
测试还覆盖慢 reader 场景：接收队列满后应产生可取消的背压，reader 恢复消费后所有 stream 数据仍须完整、按序交付。
畸形 frame、底层写失败、阻塞写 deadline 和并发关闭状态也包含在本地测试中。

```
go test ./proxy/session -count=1 -v
```

## 竞态检查

```
go test -race ./...
```

## 部署脚本检查

脚本静态检查和语法检查：

```
bash -n scripts/install-anytls-server.sh scripts/install-dedup.sh scripts/bootstrap-anytls-server.sh scripts/test-installation.sh
shellcheck scripts/install-anytls-server.sh scripts/install-dedup.sh scripts/bootstrap-anytls-server.sh scripts/test-installation.sh
```

服务端 fallback 本地测试在 `cmd/server` 中覆盖：明文 TCP 探测应被转发到 fallback，并且首包数据不会丢失。
认证测试覆盖请求被拆成多次读取，以及认证失败数据完整回放到 fallback。

```
go test ./cmd/server -count=1 -v
go test ./cmd/client -count=1 -v
./scripts/test-installation.sh
```

客户端测试还覆盖一次性 `-probe` 模式。实际安装时，该模式会在本机创建临时 echo 目标，经 TLS、AnyTLS 认证、stream 和服务端出站完成随机 payload 往返，不占用默认 SOCKS 端口。

`-probe-dedup` 经同一 AnyTLS 通道访问保留目标 `dedup.anytls.arpa/health`，要求 HTTP 204 与有效 epoch；安装器还要求该 epoch 与本机解码端一致。测试覆盖错误状态、缺失或重复 epoch、TLS 校验、超时和资源释放。

安装流程测试模拟 systemctl 和防火墙，覆盖默认启用、显式关闭与保留状态、菜单开关、旧 bootstrap 显式更新和菜单迁移、双服务独立状态回滚，以及健康/探针/文件替换失败。它不验证真实 cgroup 或系统服务恢复，后者需在隔离的 systemd 环境补验。

防火墙隔离同时覆盖 iptables、UFW 和 firewalld；测试不会根据宿主机的启用状态选择真实工具。测试仅使用 bootstrap 已安装的系统工具，不依赖开发机上的 `rg`。候选 worktree 使用 detached HEAD，属于正常的源码暂存状态。

## 真实 systemd 解码链路回归

`scripts/test-systemd-dedup.py` 需要一次性 systemd Docker 容器，其中已用默认路径安装当前源码的主服务、探针和解码端；容器还需 Python 3、openssl、可写的 cgroup v2，以及只读挂载到 `/src` 的当前仓库。测试调用实际安装的探针程序作为本机代理，使用容器内合成 HTTPS origin，检查正文哈希、并发、引用节省、流式排空、崩溃恢复、冷缓存和普通代理隔离。

```bash
docker exec -e ANYTLS_SYSTEMD_TEST=1 anytls-systemd-validation \
  python3 -u /src/scripts/test-systemd-dedup.py
```

此脚本会临时修改容器内解码 unit 并重启/停止/杀死解码进程；结束时删除测试 override、证书和日志，重启解码器恢复原 unit。它检查 Docker、PID 1 和显式测试开关，不能在生产机执行。构建/安装、开关和旧版升级仍由上面的安装流程测试覆盖。

## 性能基线

本地 benchmark 用于观察实现改动是否影响写帧路径和 PaddingScheme 生成成本。

```
go test -run '^$' -bench . -benchmem ./proxy/session
go test -run '^$' -bench BenchmarkTLSHandshake -benchmem ./util
```

Session benchmark 覆盖 Stream 读写、完整 frame 接收、空闲池和 padding RNG；util benchmark 覆盖本地完整/恢复 TLS 握手。它们不经过公网链路，因此只能反映实现开销，不能代表真实线路吞吐。

自动对比基线提交与当前工作区：

```
scripts/benchmark-before-after.sh
```

完整结果和口径见 [性能优化前后对比](./performance-comparison.md)。

## AnyTLS vs Vision 实测对比

如果要评估“同一台服务器上 AnyTLS 比 Vision 快多少”，建议在客户端侧测，而不是在 VPS 本机测。原因是代理协议的实际体验主要由客户端到 VPS 的公网路径、TLS 握手、复用策略、客户端实现和目标站点共同决定；在服务器本机测只能得到本地回环结果，容易高估真实速度。

仓库提供了一个通用对比脚本：

```
scripts/compare-proxies.sh
```

默认假设：

- AnyTLS 客户端本地代理：`socks5h://127.0.0.1:1080`
- Vision 客户端本地代理：`socks5h://127.0.0.1:1081`
- 延迟/首包测试：`https://www.cloudflare.com/cdn-cgi/trace`
- 下载测试：`https://speed.cloudflare.com/__down?bytes=52428800`

推荐测试方法：

1. 在同一台 VPS 上分别部署 AnyTLS 服务端和 Vision 节点。
2. 在同一台客户端机器上启动两个本地入口，例如 AnyTLS 监听 `127.0.0.1:1080`，Vision 监听 `127.0.0.1:1081`。
3. 确认两者走同一台 VPS、同一条客户端网络、同一个目标测试 URL。
4. 运行：

```
scripts/compare-proxies.sh --runs 10
```

如果本地端口不同：

```
scripts/compare-proxies.sh \
  --anytls socks5h://127.0.0.1:1080 \
  --vision socks5h://127.0.0.1:1081 \
  --runs 10
```

脚本会输出每组平均 TTFB、总耗时、下载 Mbps，并保存 TSV 原始结果。最终的百分比只代表这台客户端、这台 VPS、这组测试 URL 下的结果，不应直接当作所有线路上的固定结论。

为了减少误差：

- 不要同时跑大流量任务。
- 两个节点尽量使用同一个远端服务器、同一个出口网络和相近端口。
- 多跑几轮，观察中位附近的稳定趋势，不要只看一次峰值。
- 如测试大带宽，把 `--download-url` 的 bytes 调大，例如 `104857600` 或 `268435456`。
