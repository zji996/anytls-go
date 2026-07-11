# zji-dev 当前状态

本文记录当前 `zji-dev` 分支的实现、部署和验证现状，便于上线前快速确认。

## 可用性状态

当前分支已经可以用于 Linux VPS 上的服务端部署和基础验证：

- 默认从 `zji-dev` 分支现场构建 `anytls-server`。
- 支持菜单式安装、更新、状态查看、自检、重启和卸载。
- 支持完全非交互安装，未传密码时自动生成强随机密码。
- 默认监听 `0.0.0.0:8443`。
- 默认输出可复制的 AnyTLS URI，适合导入支持 AnyTLS 的客户端。
- 默认 fallback 到 `127.0.0.1:80`，用于认证失败或明文 TCP 探测。

一键交互安装：

```
curl -fsSL https://raw.githubusercontent.com/zji996/anytls-go/zji-dev/scripts/bootstrap-anytls-server.sh | sudo bash
```

一键非交互安装：

```
curl -fsSL https://raw.githubusercontent.com/zji996/anytls-go/zji-dev/scripts/bootstrap-anytls-server.sh | sudo bash -s -- install --non-interactive
```

安装后自检：

```
sudo /opt/anytls-go/scripts/install-anytls-server.sh doctor
```

## 协议兼容状态

当前优化没有改变 AnyTLS wire format：

- frame 头格式未变。
- command 编号和含义未变。
- PaddingScheme 语法未变。
- `cmdSettings` / `cmdServerSettings` 协商语义未变。
- TCP 和 UDP-over-TCP 代理语义未变。

因此目标仍是兼容现有 AnyTLS 实现，例如 anytls-go、sing-box、mihomo 和支持 AnyTLS URI 的客户端。实际互通仍建议在目标客户端版本上做一次连接验证。

## 已完成优化

- 大块 `Stream.Write` 按 65535 字节上限拆分，避免 frame 长度截断。
- 客户端先注册 stream 再发送 `cmdSYN`，降低快速 `cmdSYNACK` 竞态。
- 控制帧写 deadline 收敛到写锁内，降低并发写竞争风险。
- 服务器下发的 PaddingScheme 只更新当前 Client，不污染进程全局默认值。
- PaddingScheme 在加载时预编译规则，减少运行时 split/parse/分配。
- frame 编码逻辑集中到 `proxy/session/frame.go`。
- 每个 stream 增加有界接收队列，降低单个慢 reader 阻塞整个 session 的概率。
- 接收队列溢出时淘汰单个 stream，避免反压扩散到同一 session 的其他 stream。
- 数据 frame 写失败后立即关闭 session，避免损坏连接返回空闲池。
- Stream 写 deadline 会中断阻塞写，Stream 终止状态支持并发访问。
- 未知 command、非法 command data 和越界 PaddingScheme 会被拒绝。
- 服务端初始连接阶段有统一超时，认证请求支持跨多次读取组装。
- Session 接收数据直接使用池化 buffer，移除了每帧等长堆分配和额外复制。
- Stream 不再创建内部 pipe goroutine，并实现 sing ExtendedBuffer/headroom 快速路径。
- 普通 Stream 写和完整 Session 收帧稳定路径达到 0 B/op、0 allocs/op。
- 空闲 Session 池改为内部最大堆，移除了 `stl4go` 依赖。
- Padding 随机数使用 Session 级 ChaCha8，客户端支持 TLS session cache 和可选预热。
- 临时 TLS 证书使用 ECDSA P-256。
- 服务端支持认证失败 fallback。
- 服务端支持明文 TCP 探测 fallback，首包会转发到 fallback 后端。
- 部署脚本支持 TUI 菜单、随机密码、状态查看、更新、卸载和 `doctor` 自检。
- 部署脚本会尝试自动放行本机防火墙监听端口，并在 `doctor` 中检查规则状态。
- 提供客户端侧 AnyTLS/Vision 对比脚本，方便在同一台 VPS 上实测首包和吞吐差异。

## 已验证项目

本机已通过以下检查：

```
bash -n scripts/install-anytls-server.sh scripts/bootstrap-anytls-server.sh
shellcheck scripts/install-anytls-server.sh scripts/bootstrap-anytls-server.sh
go test ./...
go vet ./...
go test -race ./...
go test -run '^TestPlainTCPProbeFallsBack$' ./cmd/server -count=1 -v
go test -run '^$' -bench . -benchmem -count=3 ./proxy/session
bash -n scripts/install-anytls-server.sh scripts/bootstrap-anytls-server.sh scripts/compare-proxies.sh
shellcheck scripts/install-anytls-server.sh scripts/bootstrap-anytls-server.sh scripts/compare-proxies.sh
```

本机 benchmark 当前可作为实现层回归基线：

| 项目 | 当前范围 |
|--|--:|
| Stream 16 KiB 写 | 约 1.94-2.02 us/op，0 B/op，0 allocs/op |
| Stream 16 KiB 队列读 | 约 0.20 us/op，0 B/op，0 allocs/op |
| Session 完整收帧 16 KiB | 约 2.39-2.41 us/op，0 B/op，0 allocs/op |
| Session RNG PaddingSizes | 约 10 ns/op，0 B/op，0 allocs/op |
| 空闲 Session 池取还 | 约 57 ns/op，0 B/op，0 allocs/op |
| 本地 TLS 完整/恢复握手 | 约 487-540 us / 435-449 us |

benchmark 不经过真实 TLS、公网链路或目标 VPS，只用于观察本地实现开销。

AnyTLS 与 Vision 的真实速度差异需要从客户端侧连接同一台 VPS 实测。当前仓库提供 `scripts/compare-proxies.sh`，默认对比 `socks5h://127.0.0.1:1080` 和 `socks5h://127.0.0.1:1081`，输出平均 TTFB、总耗时、下载 Mbps 和百分比差异。详见 [本地测试](./testing.md)。

## 当前限制

- 示例服务端仍默认使用临时自签 TLS 证书；客户端 URI 默认带 `insecure=1`。
- 还没有实现加载正式 TLS 证书的服务端参数。
- 自签证书是进程启动时生成的短期证书，不是自动续签的正式证书机制。
- fallback 默认只负责转发；如果要让主动探测看到正常网页，需要在本机 `127.0.0.1:80` 运行 nginx、Caddy 或其他 HTTP 服务。
- 脚本无法自动修改云厂商安全组；需要手动放行服务端 TCP 端口。
- Xray 当前不能把 AnyTLS 作为原生协议直接使用，可通过本机 SOCKS/HTTP 与 `anytls-client` 组合。

## 上线前建议

- 在目标 VPS 上执行一次安装和 `doctor` 自检。
- 确认云安全组和系统防火墙放行监听端口。
- 如果开启 fallback，确认 `127.0.0.1:80` 有真实 HTTP 服务。
- 用目标客户端实际导入安装脚本输出的 URI，做一次连接测试。
- 如果需要严格 TLS 校验，先补正式证书加载能力，再把客户端 `insecure` 改为 `false`。
