# 优化策略

`zji-dev` 分支的优化目标是保持 AnyTLS 的速度特征，同时提升实现稳定性和特征安全可调性。

## 兼容性边界

默认不修改以下 wire format：

- frame 头格式：`command + streamId + data length + data`
- command 编号和含义
- `cmdSettings` / `cmdServerSettings` 的版本协商语义
- PaddingScheme 现有语法
- TCP 和 UDP-over-TCP 的代理语义

只要不改变这些边界，优化通常可以和现有 anytls-go、sing-box、mihomo 等实现保持互通。

## 速度优先原则

AnyTLS 的主要速度优势来自 session 复用：多个短连接可以复用已有 TLS 连接，减少重复握手成本。优化时应优先保护这些路径：

- `OpenStream` 延迟
- `Stream.Write` / `Stream.Read` 的分配和锁开销
- 空闲 session 池的复用效率
- padding 关闭或轻量开启时的吞吐

性能相关改动前后应至少运行：

```
go test -run '^$' -bench . -benchmem ./proxy/session
```

## 特征安全原则

特征安全不应硬编码成单一行为。不同使用场景对速度和隐蔽性的取舍不同，推荐后续按 profile 设计：

- `fast`：速度优先，轻量 padding，适合延迟敏感或带宽敏感场景。
- `balanced`：默认推荐，保留首批上行包长度处理，并避免过度填充。
- `stealth`：特征安全优先，可增加下行 padding、随机化策略或轻微时序扰动，但必须明确性能代价。

任何会明显增加延迟、带宽浪费或队头阻塞风险的策略，都不应默认开启。

## 当前已完成的优化

- 大块 `Stream.Write` 按 frame 长度上限拆分，避免 `uint16` 截断。
- 客户端先注册 stream 再发送 `cmdSYN`，降低快速 `cmdSYNACK` 竞态。
- 控制帧写 deadline 收敛到连接写锁内，降低并发写竞争风险。
- PaddingScheme 更新收敛到 Client 实例，不污染进程全局默认值。
- PaddingScheme 在加载时预编译规则，运行时不再重复 split/parse。
- frame 编码逻辑集中到 `frame.go`，减少数据帧和控制帧重复实现。
- 接收侧增加每 stream 有界队列，吸收 reader 的短时调度抖动并限制内存占用。
- 接收队列满时对 session 接收施加可取消的背压，不丢弃 stream 或伪装成正常结束，确保大流量传输完整交付。
- 数据帧写失败会立即关闭 session，避免复用已经产生半帧的连接。
- Stream 写 deadline 可以中断正在等待或执行的底层写入。
- frame command/length 和 PaddingScheme 范围增加严格校验。
- 服务端初始探测、TLS 握手和认证阶段增加超时，认证支持分段读取。
- Stream 关闭状态改为并发安全访问。
- Session 接收 buffer 直接转移给 Stream，消费后归还池，移除每帧等长堆分配和一次完整拷贝。
- Stream 直接消费有界队列，不再为每条 Stream 创建内部 pipe goroutine。
- Stream 实现 sing `ReadBuffer` / `WriteBuffer` 和 frame headroom 接口，发送侧可以原地添加 frame header。
- 普通 Stream 写帧改为直接使用字节池，稳定路径不再产生堆分配。
- 空闲 Session 池从通用 SkipList 改为无节点分配的内部最大堆，并移除 `stl4go` 依赖。
- 随机 padding 改为每 Session 一次系统熵播种 ChaCha8，并复用固定 scratch。
- 客户端启用 TLS session cache，可选 `-prewarm` 预建空闲 Session。
- 临时服务端证书改用 ECDSA P-256，减少完整 TLS 握手的签名成本和证书体积。
- 服务端支持认证失败 fallback 和明文 TCP 探测 fallback。
- 部署脚本支持菜单、随机密码、更新、状态查看、卸载和 `doctor` 自检。
- 本地 `net.Pipe` 测试覆盖 session 往返、padding 更新、握手失败传播和慢 reader 隔离。

## 当前性能数据

以下数据来自本机 `go test -run '^$' -bench ... -benchmem ./proxy/session`，仅用于实现层回归对比，不代表公网吞吐。

| 项目 | 本轮优化前 | 当前 |
|--|--:|--:|
| Stream 16 KiB 写 | 约 1.99 us/op，64 B/op，1 alloc/op | 约 1.93 us/op，0 B/op，0 allocs/op |
| Stream 16 KiB 队列读 | 约 0.76 us/op | 约 0.20 us/op，0 B/op，0 allocs/op |
| Session 完整收帧 16 KiB | 约 7.77 us/op，16.5 KiB/op，1 alloc/op | 约 2.38 us/op，0 B/op，0 allocs/op |
| Session RNG PaddingSizes | 约 262 ns/op，16 B/op，1 alloc/op | 约 10 ns/op，0 B/op，0 allocs/op |
| 空闲 Session 池取还 | 约 88 ns/op，约 71 B/op，2 allocs/op | 约 58 ns/op，0 B/op，0 allocs/op |
| 本地 TLS 完整/恢复握手 | 约 1.02 ms / 0.44 ms | 约 0.49 ms / 0.44 ms |

更完整的当前状态见 [zji-dev 当前状态](./status.md)。

同机、同口径的优化前后五轮数据见 [性能优化前后对比](./performance-comparison.md)。

## 后续可做但需要单独评估的优化

- 逐 stream 滑动窗口：需要扩展协议，让发送端只暂停慢 stream，避免当前 TCP 背压影响同一 session 的其他 stream。
- 下行 padding：可用现有 `cmdWaste` 实现，但应默认关闭或只在 `stealth` profile 中开启。
- padding profile：以配置方式切换速度/安全取舍，不改变协议格式。
- 本地 TCP + TLS 持续吞吐 benchmark：当前 net.Pipe 和 TLS 握手基准已覆盖，仍需补长期本地回环与真实 VPS 数据。
