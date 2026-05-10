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
测试还覆盖慢 reader 场景：一个 stream 的接收队列被填满前，不应阻塞其他 stream 的数据交付。

```
go test ./proxy/session -count=1 -v
```

## 竞态检查

```
go test -race ./proxy/session ./proxy/pipe ./proxy/padding
```

## 部署脚本检查

脚本静态检查和语法检查：

```
bash -n scripts/install-anytls-server.sh scripts/bootstrap-anytls-server.sh
shellcheck scripts/install-anytls-server.sh scripts/bootstrap-anytls-server.sh
```

服务端 fallback 本地测试在 `cmd/server` 中覆盖：明文 TCP 探测应被转发到 fallback，并且首包数据不会丢失。

```
go test ./cmd/server -count=1 -v
```

## 性能基线

本地 benchmark 用于观察实现改动是否影响写帧路径和 PaddingScheme 生成成本。

```
go test -run '^$' -bench . -benchmem ./proxy/session
```

建议在做性能相关改动前后各跑一次，并保留输出用于对比。当前 benchmark 不经过真实 TLS 和公网链路，因此它只能反映本地实现开销，不能代表真实线路吞吐。

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
