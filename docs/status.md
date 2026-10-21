# zji-dev 当前状态

本文记录当前 `zji-dev` 分支的实现、部署和验证现状，便于上线前快速确认。

## 云端安装环境修复（2026-10-22）

修复安装测试未隔离宿主 UFW / firewalld、以及依赖开发工具 `rg` 的问题；测试失败现在报告具体行号，正常候选 worktree 不再输出分支告警。bootstrap 自动补齐系统工具、账号管理、网络检查和 systemd 依赖，并在候选测试前检查必要命令。

已通过模拟宿主 UFW 活跃的完整回归（确认未调用宿主防火墙）、root / 无 rg / detached worktree / umask 077 环境下的完整候选检查，以及隔离 Debian 容器内实际 apt 安装与依赖预检。dnf / yum / apk 的包选择和失败处理已用模拟命令验证，未在这些发行版上实际安装。ShellCheck 与 shell 语法检查通过；本轮不涉及 Go 实现修改。

## 默认解码器安装（2026-10-21）

默认安装请求去重解码端的实现已完成：新装及没有保存设置的旧版升级默认开启，明确关闭后保持关闭；支持 `--with-dedup` / `--without-dedup` 和菜单开关。主服务、健康探针、解码端及配置由同一事务安装，任何启用检查失败都会回滚。最新 bootstrap 的显式更新和交互菜单均使用候选版本协调器，避免旧安装器遗漏解码组件。

本地已通过全量 Go 测试、竞态检查、`go vet`、ShellCheck、shell 语法检查和隔离安装流程测试。安装测试包括旧版迁移、菜单入口、fetch 失败、显式关闭保留及双服务独立状态回滚。

另外已在一次性 Docker 容器内使用真实 systemd 和实际安装的程序验证：

- 两个服务启动，解码健康检查与 AnyTLS 端到端探针通过，cgroup 实际硬上限为 524288000 字节（500 MiB）。
- 顺序普通/解码请求复用同一 TLS session；并发合成请求正文哈希一致，重复正文以引用还原，观察到解码帧字节数低于原始正文的一半。
- 流式响应首段及时到达，解码器正常重启排空期间尾段完整，重启后缓存清零。
- SIGKILL 后解码器自动恢复、epoch 改变；旧缓存引用返回重置提示，重新发送原文成功。
- 停止和崩溃解码器均未改变主服务 PID；解码器停止时普通代理仍能传输。

真实服务回归脚本为 `scripts/test-systemd-dedup.py`，只允许在显式启用测试的 systemd Docker 容器中执行。合成 origin 及临时 unit override 会在结束时清理；它不访问真实 OpenAI 业务。

生产服务器更新由操作者执行，本轮未更新云端，也未验证路由器设备名单、客户端 CA 信任、真实业务或生产去重收益。更新命令及验收步骤见 [去重解码端安装与迁移](./dedup-installation-design.md)。

2026-10-22 前端按生产观测调整（只改前端，帧格式和解码端不变）：

- 新内容块在解码端应答前只算"在途"，并发帧不引用它们而是再带一次原文，避免引用尚未解码的块导致 409 和整包原文重发；帧丢失、误投或解码端忙未读时撤销这些块，下一轮不再因此重发。
- 记录流式写入压缩器，auto 模式重试复用同一缓冲；每个请求的在途预算从 3 倍正文降为 2 倍，8 MiB base64 正文的编码分配从约 3.19 倍降到约 1.2 倍，编码吞吐持平。
- 在途预算不足时最多等待 `-inflight-wait`（默认 3s）再放行原文，大正文原样过境通常更慢。
- 解码端不可达时仍按原逻辑改发原文：请求可能已到达 origin 时重发有重复执行的可能，但不重发会让上游网关报传输错误并自行换账号重试，同样会重复，且会临时停用账号。

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
- 每个 stream 增加有界接收队列，吸收 reader 的短时调度抖动并限制内存占用。
- 接收队列满时对 session 接收施加可取消的 TCP 背压，避免把正常大流量传输截断为成功关流。
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
- 部署脚本支持运行中覆盖、部署锁、原子文件替换、退出 trap 回滚、候选 worktree 两阶段更新、真实 AnyTLS 健康探针、状态查看、卸载和可返回失败状态的 `doctor` 自检。
- 服务使用专用系统用户和独立密码文件运行，密码不出现在 systemd `ExecStart` 参数中。
- 部署脚本会尝试自动放行本机防火墙监听端口，记录自身新增的规则，并在改端口或卸载时清理。
- 提供客户端侧 AnyTLS/Vision 对比脚本，方便在同一台 VPS 上实测首包和吞吐差异。

## 已验证项目

本机已通过以下检查：

```
bash -n scripts/install-anytls-server.sh scripts/bootstrap-anytls-server.sh scripts/test-installation.sh
shellcheck scripts/install-anytls-server.sh scripts/bootstrap-anytls-server.sh scripts/test-installation.sh
bash scripts/test-installation.sh
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
