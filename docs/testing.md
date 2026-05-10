# 本地测试

本仓库的测试尽量设计成本机可运行，不依赖外部服务器、真实 TLS 证书或公网连通性。

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

## 性能基线

本地 benchmark 用于观察实现改动是否影响写帧路径和 PaddingScheme 生成成本。

```
go test -run '^$' -bench . -benchmem ./proxy/session
```

建议在做性能相关改动前后各跑一次，并保留输出用于对比。当前 benchmark 不经过真实 TLS 和公网链路，因此它只能反映本地实现开销，不能代表真实线路吞吐。
