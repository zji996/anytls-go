# 海外服务器部署

本文面向把当前 `anytls-go` 分支直接部署到一台 Linux VPS 的场景。

当前 `zji-dev` 状态摘要：

- 可以直接在 Linux VPS 上通过 bootstrap 脚本部署服务端。
- 默认现场构建 `zji-dev`，安装后由 systemd 常驻运行。
- 默认生成随机密码、监听 `0.0.0.0:8443`、输出 AnyTLS URI。
- 默认 fallback 到 `127.0.0.1:80`，认证失败和明文 TCP 探测会转发到该后端。
- 安装后自动执行 `doctor` 自检；也可以后续手动运行。
- 协议 wire format 未改变，兼容目标不变。

更完整的状态、验证结果和限制见 [zji-dev 当前状态](./status.md)。

## 推荐一键安装

默认部署 `zji-dev` 分支，并在服务器现场构建：

```
curl -fsSL https://raw.githubusercontent.com/zji996/anytls-go/zji-dev/scripts/bootstrap-anytls-server.sh | sudo bash
```

完全非交互安装：

```
curl -fsSL https://raw.githubusercontent.com/zji996/anytls-go/zji-dev/scripts/bootstrap-anytls-server.sh | sudo bash -s -- install --non-interactive
```

这个 bootstrap 脚本会：

- 安装基础依赖：`ca-certificates`、`curl`、`git`、`tar`、`gzip`。
- 如果服务器没有 Go，则安装脚本内指定的 Go 版本。
- clone 或更新 `https://github.com/zji996/anytls-go.git` 的 `zji-dev` 分支到 `/opt/anytls-go`。
- 执行 `go mod download`，提前下载 Go modules。
- 启动菜单式服务端管理器，现场构建并安装 `anytls-server`。
- 尝试通过本机防火墙放行监听端口。
- 安装完成后执行自检，显示服务状态、监听端口、fallback 状态和客户端 URI。

默认交互项尽量少：

- 监听地址默认 `0.0.0.0:8443`。
- 客户端 URI 的服务器地址默认自动探测公网 IP。
- 密码默认自动生成强随机值，直接回车即可。
- 认证失败 fallback 默认反代到 `127.0.0.1:80`。
- PaddingScheme 默认不自定义。
- 安装完成后输出 AnyTLS URI，方便复制到支持 AnyTLS 的客户端。

可通过环境变量覆盖默认值：

```
curl -fsSL https://raw.githubusercontent.com/zji996/anytls-go/zji-dev/scripts/bootstrap-anytls-server.sh | sudo ANYTLS_BRANCH=zji-dev ANYTLS_SRC_DIR=/opt/anytls-go bash
```

## 服务器需要什么

bootstrap 自动安装基础依赖后，服务器最终需要：

- Linux amd64/arm64 服务器。
- systemd，用于常驻运行 `anytls-server`。
- Go 1.24 或更新版本，用于在服务器上从源码构建。bootstrap 会自动安装 Go；如果使用预编译二进制部署，则服务器不需要 Go。
- 一个 TCP 端口，例如 `8443/tcp` 或 `443/tcp`。

可选依赖：

- `curl`：安装脚本用于自动探测公网 IP。
- `python3`：安装脚本用于对 URI 密码做百分号编码；没有时也能继续输出未编码密码。
- `ufw`、`firewalld` 或云厂商安全组：用于放行服务端口。

当前示例服务端会自动生成临时自签 TLS 证书，适合快速部署和测试。生产部署如果需要严格 TLS 证书校验，应改造服务端 TLS 配置，加载正式证书。

## 仓库内安装脚本

在服务器上 clone 仓库后执行：

```
sudo ./scripts/install-anytls-server.sh
```

无参数运行时会进入交互式向导。也可以非交互执行：

```
sudo ./scripts/install-anytls-server.sh -p '你的密码' -l 0.0.0.0:8443 -s your.server.name --branch zji-dev --non-interactive
```

参数说明：

- `-p` / `--password`：AnyTLS 密码。交互模式可回车使用随机密码；非交互模式未传时也会自动生成。
- `-l` / `--listen`：监听地址，默认 `0.0.0.0:8443`。
- `-s` / `--server-name`：生成客户端 URI 时使用的服务器域名或 IP。
- `--branch`：源码构建时期望的 git 分支，默认 `zji-dev`。
- `--fallback`：认证失败时反代的地址，默认 `127.0.0.1:80`。
- `--binary`：使用已有 `anytls-server` 二进制安装，跳过服务器现场编译。
- `--padding-scheme`：可选，安装自定义 PaddingScheme 文件。
- `--no-firewall`：不自动修改本机防火墙规则。
- `--non-interactive`：不提示输入；未传密码时自动生成随机密码。

脚本会安装：

- `/usr/local/bin/anytls-server`
- `/etc/anytls/server.env`
- `/etc/systemd/system/anytls-server.service`

常用管理命令：

```
sudo /opt/anytls-go/scripts/install-anytls-server.sh
sudo systemctl status anytls-server
sudo /opt/anytls-go/scripts/install-anytls-server.sh doctor
sudo systemctl restart anytls-server
sudo journalctl -u anytls-server -f
```

菜单功能：

- 安装 / 重装服务端。
- 更新 `zji-dev`、重新构建并重启。
- 查看 systemd 状态和当前客户端 URI。
- 自检服务状态、二进制、配置、监听端口和 fallback 目标。
- 重启服务。
- 卸载服务和配置。

安装脚本会尽量自动放行本机防火墙：

- `ufw` 已启用时执行 `ufw allow PORT/tcp`。
- `firewalld` 已运行时执行永久端口规则并 reload。
- 没有 `ufw` / `firewalld` 但存在 `iptables` 时，添加运行时 ACCEPT 规则；这类规则可能不会在重启后保留。

云厂商安全组或供应商防火墙无法从 VPS 内可靠修改，仍需要在控制台手动放行对应 TCP 端口。若不希望脚本修改本机防火墙，可加 `--no-firewall`。

## 现场编译还是拷贝二进制

当前仓库编译资源消耗较小，因此默认推荐服务器现场编译 `zji-dev`，这样部署路径最直接，也方便持续更新开发分支。若你需要完全固定已测试的二进制，仍可以本机或 CI 编译好再拷贝到服务器。

本机实测当前分支构建资源，仅供估算：

| 场景 | 耗时 | 峰值 RSS | 额外缓存/下载 |
|--|--:|--:|--:|
| 热构建 server | 约 7.8s | 约 34MB | 已有缓存 |
| 热构建 client | 约 2.0s | 约 40MB | 已有缓存 |
| 全冷构建 server | 约 11.3s | 约 37MB | module cache 约 15MB，build cache 约 89MB |

产物大小：

- `anytls-server` 约 5.4MB
- `anytls-client` 约 6.3MB

本机编译 server：

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o anytls-server ./cmd/server
```

拷贝并安装：

```
scp anytls-server root@your.server:/tmp/anytls-server
sudo ./scripts/install-anytls-server.sh -p '你的密码' -l 0.0.0.0:8443 -s your.server.name --binary /tmp/anytls-server
```

如果你的服务器也是 Linux amd64，直接使用上面的命令即可。arm64 服务器则把 `GOARCH=amd64` 改成 `GOARCH=arm64`。

## 客户端连接

示例客户端：

```
./anytls-client -l 127.0.0.1:1080 -s 'anytls://password@host:8443/?insecure=1'
```

如果使用当前示例服务端的自签证书，客户端需要 `insecure=1` 或命令行默认的不安全 TLS 模式。若后续服务端支持正式证书，可以使用 `insecure=0` 并设置正确的 `sni`。

## 和 sing-box 的兼容性

当前 `zji-dev` 分支保持 AnyTLS wire format 兼容：frame 格式、command 编号、PaddingScheme 语法和 v1/v2 协商均未改变。因此协议层目标是继续兼容实现了 AnyTLS 的客户端和服务端。

sing-box 官方文档列出了 `anytls` inbound 和 outbound，并说明 AnyTLS outbound 自 sing-box 1.12.0 起可用。使用 sing-box 连接本服务端时，配置应使用 `type: "anytls"`、相同 `password`，并按服务端证书情况配置 TLS 的 `insecure` / `server_name`。

示例 outbound：

```json
{
  "type": "anytls",
  "tag": "anytls-out",
  "server": "your.server.name",
  "server_port": 8443,
  "password": "你的密码",
  "idle_session_check_interval": "30s",
  "idle_session_timeout": "30s",
  "min_idle_session": 5,
  "tls": {
    "enabled": true,
    "server_name": "your.server.name",
    "insecure": true
  }
}
```

如果你的服务端使用示例自签证书，`insecure` 需要为 `true`。如果使用正式证书，应改为 `false`。

## 和 Xray 的兼容性

截至本文档编写时，Xray 官方协议列表未包含 AnyTLS。Xray 可以继续作为本机 SOCKS/HTTP 入站或其他链路的一部分，但不能直接把 AnyTLS 当作 Xray 原生 inbound/outbound 使用。

可行组合是：

- `anytls-client` 在本机提供 SOCKS/HTTP 入站。
- Xray 客户端使用 SOCKS/HTTP outbound 指向 `anytls-client`。
- 或使用 sing-box 作为 AnyTLS 客户端/服务端组件。

## 和 Vision 节点做速度对比

AnyTLS 的速度优势主要来自 session 复用，理论上对短连接、频繁建连、首包延迟更敏感的场景更有利。Vision 的优势和瓶颈则取决于 Xray 配置、TLS/REALITY 设置、客户端实现和线路质量。要判断你自己的服务器上谁更快，应在客户端侧对同一台 VPS 做实测。

准备方式：

- 同一台 VPS 上同时部署 AnyTLS 服务端和 Vision 节点。
- 客户端机器上分别启动两个本地代理入口，例如 AnyTLS `127.0.0.1:1080`，Vision `127.0.0.1:1081`。
- 两个节点尽量使用同一 VPS、同一网络出口、相同测试 URL，避免把线路波动误判为协议差异。

运行仓库内脚本：

```
scripts/compare-proxies.sh --runs 10
```

指定不同本地入口：

```
scripts/compare-proxies.sh \
  --anytls socks5h://127.0.0.1:1080 \
  --vision socks5h://127.0.0.1:1081 \
  --runs 10
```

脚本会输出平均 TTFB、总耗时、下载 Mbps 和 AnyTLS 相对 Vision 的百分比差异，并保存 TSV 原始结果。这个结果是实际线路数据，比本机 benchmark 更能回答“快多少”；但它只代表当前客户端、VPS 和目标站点组合。

## 安全和特征建议

- 密码使用随机长字符串。
- 优先使用 443/tcp 或常见 HTTPS 端口，但要确认服务器上没有其他服务占用。
- 默认 PaddingScheme 只是示例；如果担心固定特征，建议使用自定义 PaddingScheme，并保留兼容语法。
- fallback 默认转发到 `127.0.0.1:80`。如果要让主动探测看到正常网站，请在本机 80 端口运行 nginx/Caddy/静态站点；如果不需要 fallback，可在安装时把 fallback 设置为空。
- 当前服务端会把明文 TCP 探测直接转发到 fallback；TLS ClientHello 仍先进入 TLS 握手，AnyTLS 认证失败后再转发到 fallback。这不改变 AnyTLS 协议格式。
- 当前 `zji-dev` 的优化没有改变协议线格式；接收队列、分片、padding 作用域等都是实现层优化。
