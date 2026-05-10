# AnyTLS

一个试图缓解 嵌套的TLS握手指纹(TLS in TLS) 问题的代理协议。`anytls-go` 是该协议的参考实现。

- 灵活的分包和填充策略
- 连接复用，降低代理延迟
- 简洁的配置

[用户常见问题](./docs/faq.md)

[协议文档](./docs/protocol.md)

[URI 格式](./docs/uri_scheme.md)

[本地测试](./docs/testing.md)

[优化策略](./docs/optimization.md)

[海外服务器部署](./docs/deployment.md)

[zji-dev 当前状态](./docs/status.md)

## 快速食用方法

为了方便，示例服务器和客户端默认采用不安全的配置，该配置假设您不会遭遇 TLS 中间人攻击（这种情况偶尔发生在网络接入层，在骨干网络上几乎不可能实现）；否则，您的通信内容可能会被中间人截获。

### 海外服务器一键安装

`zji-dev` 分支提供服务端管理脚本，默认会拉取并现场编译 `zji-dev`：

```
curl -fsSL https://raw.githubusercontent.com/zji996/anytls-go/zji-dev/scripts/bootstrap-anytls-server.sh | sudo bash
```

也可以完全非交互安装，密码会自动生成并在安装完成后输出客户端 URI：

```
curl -fsSL https://raw.githubusercontent.com/zji996/anytls-go/zji-dev/scripts/bootstrap-anytls-server.sh | sudo bash -s -- install --non-interactive
```

脚本会打开菜单式向导。默认值尽量自动化：

- 监听地址默认 `0.0.0.0:8443`，直接回车即可。
- 客户端 URI 的服务器地址默认自动探测公网 IP。
- 密码默认自动生成强随机值，直接回车即可。
- fallback 默认指向 `127.0.0.1:80`，认证失败或明文探测会转发到本机 HTTP 服务。
- PaddingScheme 默认不自定义。

脚本会自动完成：

- 安装基础依赖和 Go。
- clone / 更新 `https://github.com/zji996/anytls-go.git` 的 `zji-dev` 分支。
- 执行 `go mod download` 预下载依赖。
- 从 `zji-dev` 源码构建 `anytls-server`。
- 写入 systemd service 并启动服务。
- 尝试通过本机 `ufw`、`firewalld` 或 `iptables` 放行监听端口。
- 执行部署自检，检查服务状态、监听端口和 fallback 目标。
- 输出可复制到 sing-box、Shadowrocket 等客户端的 AnyTLS URI。

如果要和同服务器上的 Xray/VLESS Vision 节点做速度对比，可在客户端侧使用：

```
scripts/compare-proxies.sh --runs 10
```

默认对比 `socks5h://127.0.0.1:1080` 的 AnyTLS 和 `socks5h://127.0.0.1:1081` 的 Vision，输出平均 TTFB、下载 Mbps 和百分比差异。具体准备方式见 [本地测试](./docs/testing.md#anytls-vs-vision-实测对比)。

脚本也可用于后续管理：

```
sudo /opt/anytls-go/scripts/install-anytls-server.sh
```

菜单支持安装/重装、更新 `zji-dev` 并重启、查看状态和客户端 URI、自检、重启、卸载。

安装脚本会尽量自动放行服务器本机防火墙，但云厂商安全组或供应商防火墙仍需要在控制台放行对应 TCP 端口。

### 示例服务器

```
./anytls-server -l 0.0.0.0:8443 -p 密码
```

`0.0.0.0:8443` 为服务器监听的地址和端口。

### 示例客户端

```
./anytls-client -l 127.0.0.1:1080 -s 服务器ip:端口 -p 密码
```

`127.0.0.1:1080` 为本机 Socks5 代理监听地址，理论上支持 TCP 和 UDP(通过 udp over tcp 传输)。

v0.0.12 版本起，示例客户端可直接使用 URI 格式:

```
./anytls-client -l 127.0.0.1:1080 -s "anytls://password@host:port"
```

如果 URI 省略端口，示例客户端会使用默认端口 443。示例客户端默认允许不安全 TLS 连接；需要启用证书校验时可以使用 `-insecure=false`，或在 URI 中写 `?insecure=0`。

### zji-dev 兼容性优化

`zji-dev` 分支保持协议 wire format 兼容，不修改 frame 格式、command 编号、PaddingScheme 语法和 v1/v2 协商规则。当前优化集中在实现层：

- 大块 Stream 数据会按 65535 字节上限拆分为多个 `cmdPSH`，避免 frame 长度截断。
- 客户端先注册本地 stream 再发送 `cmdSYN`，减少快速 `cmdSYNACK` 回包造成的竞态。
- 服务器下发的 PaddingScheme 只更新当前 Client 实例，不影响进程内其他 Client。
- URI 行为与文档对齐，支持省略端口默认 443 和 `insecure` 参数。
- 增加了基础单元测试，覆盖分片、URI 默认端口和 Client 级 padding 更新。

当前状态详见 [zji-dev 当前状态](./docs/status.md)。简要来说，本分支已经支持 VPS 服务端一键部署、随机密码、fallback、自检和本机测试矩阵；仍默认使用自签 TLS 证书，生产环境如需严格证书校验应继续补正式证书加载能力。

### sing-box

https://github.com/SagerNet/sing-box

它包含了 anytls 协议的服务器和客户端。

### mihomo

https://github.com/MetaCubeX/mihomo

它包含了 anytls 协议的服务器和客户端。

### Shadowrocket

Shadowrocket 2.2.65+ 实现了 anytls 协议的客户端。
