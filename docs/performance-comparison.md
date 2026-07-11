# 性能优化前后对比

本文比较基线提交 `b9a46ac8fa1a0df6f7aa871f1d92ec6ee7e5e858` 与当前工作区实现。

## 测试环境

- CPU：AMD Ryzen 7 9800X3D
- OS/Arch：Linux amd64
- Go：仓库 `go.mod` 指定的 Go 1.24 工具链
- 每项执行 5 轮，表格使用 5 轮中位数
- Stream/Session 使用本地 `net.Pipe`，TLS 使用本地内存连接
- 数据块大小：16 KiB

这些 benchmark 用于比较实现开销，不代表公网吞吐或目标 VPS 的最终速度。

## 汇总结果

| 路径 | 优化前中位数 | 优化后中位数 | 变化 |
|--|--:|--:|--:|
| Stream 写 | 1991 ns/op，64 B/op，1 alloc/op | 1932 ns/op，0 B/op，0 allocs/op | 延迟 -3.0%，消除分配 |
| Stream 队列读 | 761.9 ns/op | 202.0 ns/op | 延迟 -73.5%，约 3.77x |
| Session 完整收帧 | 7773 ns/op，16506 B/op，1 alloc/op | 2379 ns/op，0 B/op，0 allocs/op | 延迟 -69.4%，约 3.27x |
| 随机 PaddingSizes | 261.9 ns/op，16 B/op，1 alloc/op | 10.08 ns/op，0 B/op，0 allocs/op | 延迟 -96.2%，约 26.0x |
| 空闲 Session 池取还 | 约 88 ns/op，约 71 B/op，2 allocs/op | 约 58 ns/op，0 B/op，0 allocs/op | 延迟约 -34%，消除分配 |
| TLS 完整握手 | 1.021 ms/op | 0.492 ms/op | 延迟 -51.8%，约 2.08x |
| TLS 恢复握手 | 0.437 ms/op | 0.441 ms/op | 基本持平，差异属于噪声范围 |

旧客户端没有配置 `ClientSessionCache`，因此实际新建 Session 走的是完整握手。当前客户端第一次连接约 0.492 ms，后续可恢复到约 0.441 ms；与旧生产路径的 1.021 ms 相比，后续握手本地 CPU 时间约降低 56.8%。公网环境还会受 RTT 和握手数据量影响。

## 原始范围

| 路径 | 优化前 5 轮范围 | 优化后 5 轮范围 |
|--|--:|--:|
| Stream 写 | 1987-1996 ns/op | 1925-1940 ns/op |
| Stream 队列读 | 754.6-766.7 ns/op | 201.7-203.6 ns/op |
| Session 完整收帧 | 7691-7870 ns/op | 2372-2412 ns/op |
| 随机 PaddingSizes | 258.9-266.0 ns/op | 10.05-10.23 ns/op |
| TLS 完整握手 | 1.012-1.031 ms/op | 0.485-0.495 ms/op |
| TLS 恢复握手 | 0.434-0.449 ms/op | 0.431-0.447 ms/op |

## 收益来源

- Session 收帧不再执行等长 `make+copy`，池化 buffer 直接转移给 Stream。
- Stream 直接消费接收队列，移除了每 Stream 的内部 pipe goroutine 和 channel 往返。
- Stream 实现 sing 的 `ReadBuffer`、`WriteBuffer` 和 frame headroom 接口。
- 普通数据帧和控制帧直接使用字节池，稳定写路径不再分配。
- 空闲 Session 池使用内部最大堆，替代会创建节点和迭代器的 SkipList。
- Padding 使用 Session 级 ChaCha8 和固定 scratch，避免每条规则调用系统随机源。
- 临时证书从 RSA 2048 改为 ECDSA P-256，客户端启用 TLS session cache。

## 复现方式

仓库提供了自动创建基线 worktree、复制兼容 benchmark、依次运行优化前后测试并清理 worktree 的脚本：

```bash
scripts/benchmark-before-after.sh
```

也可以指定其他基线提交：

```bash
scripts/benchmark-before-after.sh <git-ref>
```

跨版本公共 benchmark 位于 `proxy/session/performance_comparison_benchmark_test.go`。脚本只在临时 worktree 中复制测试文件，不修改基线提交。

## 仍需实测

- 本地 TCP 回环上的长时间 TLS 吞吐和 CPU 使用率。
- 目标 VPS 上相同线路、相同目标站点的 TTFB、持续下载和并发连接表现。
- 不同 `GOMAXPROCS`、不同 RTT 和丢包率下的 Session 队列与 TLS 恢复收益。
