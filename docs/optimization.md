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
- PaddingScheme 更新收敛到 Client 实例，不污染进程全局默认值。
- PaddingScheme 在加载时预编译规则，运行时不再重复 split/parse。
- frame 编码逻辑集中到 `frame.go`，减少数据帧和控制帧重复实现。
- 接收侧增加每 stream 有界队列，避免单个慢 reader 直接阻塞 session 接收循环。
- 本地 `net.Pipe` 测试覆盖 session 往返、padding 更新和握手失败传播。

## 当前性能数据

以下数据来自本机 `go test -run '^$' -bench ... -benchmem ./proxy/session`，仅用于实现层回归对比，不代表公网吞吐。

| 项目 | 优化前 | 当前 |
|--|--:|--:|
| 混合 PaddingSizes | 约 123 ns/op，55 B/op，2 allocs/op | 约 24 ns/op，4 B/op，0 allocs/op |
| 固定 PaddingSizes | 未单独统计 | 约 7 ns/op，0 B/op，0 allocs/op |
| 随机 PaddingSizes | 未单独统计 | 约 81 ns/op，16 B/op，1 alloc/op |
| 持久 net.Pipe 写帧 | 约 4.4-4.7 us/op，64 B/op，1 alloc/op | 约 2.8-2.9 us/op，64 B/op，1 alloc/op |

## 后续可做但需要单独评估的优化

- 接收队列策略调优：根据真实压力测试调整队列大小、内存占用和背压行为。
- 下行 padding：可用现有 `cmdWaste` 实现，但应默认关闭或只在 `stealth` profile 中开启。
- padding profile：以配置方式切换速度/安全取舍，不改变协议格式。
- 更细的 benchmark：拆分纯编码、net.Pipe、TLS、本地回环四类基线，避免把不同开销混在一起。
