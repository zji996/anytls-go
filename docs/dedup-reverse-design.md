# 去重反向入口：不拦截 TLS 的上游接入

状态：anytls-go 侧已实现（前端反向入口、解码端名单文件、安装器只重启有变化的服务）；Stargate 侧实现中。部署由操作者执行；执行前须经独立审查（deepseek）通过。

## 背景

Sub2API（8.12）的 deepseek 账号 107 直连第三方中转 `api.09050207.xyz`：24 小时 1821 个请求，每个约 9.7 万 token 上下文、约 400 KB 正文，98% 是重复的历史，却从家宽原样上传；同一请求经 DMIT anytls 出口的首字节时间约 0.45 s，直连 0.9–1.7 s。

现有接入方式是路由器 8.1 的前端按 SNI 拦截 TLS。加一个新域名要：换带名称约束的 CA，8.12 信任新 CA 时重启 Sub2API；改 `targets` 时重启 Stargate（sing-box）；改 DMIT 解码端 `-domains` 时重启解码端。每一步都打断现有服务。

## 目标

- 终态下加一条上游链路不重启任何进程：Sub2API、sing-box、前端、解码端都不重启，CA 不变。
- 复用现有部件：前端进程与它的通道（同一编码器、同一索引、同一条到解码端的 HTTP/2 链路与直连回退）、DMIT 解码端与其缓存、Stargate 的配置事务与升级脚本。
- 上线到终态只集中重启一次：DMIT 解码端换一次程序，路由器前端做一次带 `-linger` 的交接。sing-box、anytls-server、Sub2API 不重启。
- 现有拦截链路（chatgpt.com 等）的行为不变。

非目标：拦截目标（`targets`）的热更新；它仍随 Stargate 重启生效，只服务于我们控制不了 base_url 的客户端。

## 终态形态

```
Sub2API (8.12)  --http-->  前端 -reverse (8.1:10863)  --通道 main 编码-->  sing-box dedup-in
   base_url = http://192.168.8.1:10863/api.09050207.xyz/v1        ~ anytls ~  DMIT anytls-server
                                                                  -> dedup.anytls.arpa 解码端 -> https://api.09050207.xyz/v1/...
```

### 前端：反向入口（anytls-go `cmd/dedup front`）

新增参数，全部可选，不给 `-reverse` 时前端与现在完全一样：

- `-reverse ADDR`：明文 HTTP 监听，例如 `192.168.8.1:10863`。
- `-reverse-origins FILE`：允许经反向入口访问的上游主机，一行一个，`#` 起注释；SIGHUP 重读，文件解析失败时保留旧名单并记录日志。
- `-reverse-allow CIDRS`：允许的客户端源地址（逗号分隔），开启 `-reverse` 时必填；不在其内的请求得到 403。Stargate 用 `devices` 填。
- `-reverse-auth FILE`：内容为 `user:password`，即 sing-box `dedup-in` 入站的用户。反向请求用它取通道：通道按 SOCKS 用户名和密码作键，与 sing-box 送来的拦截流量取到**同一个通道**，共用编码器、索引、解码端链路。SIGHUP 同样重读。

请求形态：`http://<reverse>/<origin>/<path>?<query>` 转为 `https://<origin>/<path>?<query>`，`Host` 改为 `<origin>`，只支持 443。处理顺序：

1. 源地址不在 `-reverse-allow` 内：关闭连接。
2. 第一段路径不在 `-reverse-origins` 内：`421 Misdirected Request`。
3. 通道健康且解码端声明接受该主机：交给通道编码器，与拦截请求同一路径（`enc.ServeHTTP`）。
4. 否则交给通道的直连代理（`lane.direct`，经同一 anytls 出口直达上游，不去重）。因此：
   - 解码端暂时不可达时，请求照样完成，只是不去重；
   - 名单只在前端加了、DMIT 还没加时，链路已经可用（走 anytls、不去重），DMIT 加上后约 5 s 内自动开始去重。

升级请求（WebSocket）走第 4 条。统计里通道照常计数；另加 `reverse_requests`、`reverse_direct`、`reverse_refused`。

### 解码端：可热更新的域名名单（`cmd/dedup back`）

- 新增 `-domains-file FILE`，格式同上，SIGHUP 重读，解析失败保留旧名单；与 `-domains` 二选一。
- `/health` 增加响应头 `X-Dedup-Domains`，列出当前名单。编码端每次探测（5 s）记下它；前端第 3 步据此判断。拿不到这个头（旧解码端）时，只把前端 `-domains`（拦截名单）里的主机视为解码端接受，行为与现在一致。
- `scripts/install-dedup.sh`：名单落到 `/etc/anytls/dedup-domains`（首次从现有 `ANYTLS_DEDUP_DOMAINS` 迁移），unit 改用 `-domains-file` 并加 `ExecReload=/bin/kill -HUP $MAINPID`。原有的事务和回滚照旧。迁移后名单文件是真源：`dedup.env` 里的 `ANYTLS_DEDUP_DOMAINS` 每次安装按文件重写，改名单只改 `dedup-domains`。
- `back` 必须给 `-domains` 或 `-domains-file` 之一；两者都不给不再表示“任意主机”，而是启动失败。
- 安装器 `update`/`install`：主服务的程序、环境、密码、padding 与 unit 都与现网逐字节相同时，不重启 anytls-server（回滚时同理）。构建是可复现的（`-trimpath -buildvcs=false`），只改解码端的提交因此只重启解码端。
- SIGHUP 在函数返回前就已注册：Go 程序收到未注册的 SIGHUP 会退出，刚启动就收到 reload 也不能把它打死。现有 `decode` 模式的路由热加载原先在协程里注册，有同样的窗口，一并修正。

### Stargate（8.1）

- UCI `dedup` 段新增 `reverse_listen`（空为关闭）和 `reverse_origins`；源地址沿用 `devices`。
- `dedup.sh` 写出 `/etc/stargate/dedup/reverse-origins` 与 `reverse.auth`（0600，取自 sing-box 配置里 `dedup-in` 的用户），前端参数只含这两个**文件路径**。名单变化不改变参数，所以 procd 不会因此起新实例。
- 新命令 `dedup.sh reverse add|remove|list <host>`：改 UCI、原子替换名单文件、向前端发 SIGHUP（init 脚本 `reload_service`）。不重启 sing-box，不换 CA。
- `upgrade.sh` 支持把以上参数带进前端交接。

### Sub2API（8.12）

在管理后台把账号的 base_url 改为 `http://192.168.8.1:10863/<origin>/<原路径>`，热生效；回滚就是改回原地址。不重启 Sub2API。

## 终态下加一条链路

1. 8.1：`/usr/share/stargate/dedup.sh reverse add api.example.com`（SIGHUP，前端不重启）。
2. DMIT：名单文件加一行，`systemctl reload anytls-dedup`（SIGHUP，解码端不重启，缓存保留）。
3. Sub2API 后台改该账号 base_url。

第 2 步可以晚做：只做 1、3 时链路已经能用（走 anytls、不去重）。

## 一次性上线

首批名单（前端与解码端相同）：现有 9 个拦截域名，加上 `api.09050207.xyz`、`api.deepseek.com`、`api.cline.bot`，让 Sub2API 另外两个 deepseek 平台账号以后只改 base_url 即可接入。

顺序：

1. DMIT：先在机上用目标提交构建 `cmd/server`，与现网 `/usr/local/bin/anytls-server` 做 `cmp`，确认相同；再执行安装器 `update`：解码端换程序、写名单文件、改 unit，重启一次 `anytls-dedup`，anytls-server 因无变化不重启。
2. 8.1：`dedup.sh` 配置 `reverse_listen`、`reverse_origins`，用 `upgrade.sh --router` 交接前端（新旧实例并存，旧实例 `-linger` 后排空）。sing-box 不重启。
3. 8.12：先用 curl 经 `http://192.168.8.1:10863/api.09050207.xyz/v1/models` 验证（期望 401，与直连一致），再在 Sub2API 后台改账号 107 的 base_url，观察 `usage_logs` 与前端 `/stats`。

中断面：

- 第 1 步：解码端停止到新进程就绪之间（秒级），前端各通道改走直连，请求不失败；经解码端的在途请求有 30 s 排空，超过 30 s 的流式回复会被截断；缓存清空，每个会话的下一个请求整包过一次。
- 第 2 步：旧前端排空 30 s，超过的长流式回复被截断。
- 第 3 步：无。

建议在低峰执行；两步可以在同一窗口里连做，缓存只冷启动一次。

回滚：

- Sub2API 改回原 base_url，立即生效。
- 前端：`reverse_listen` 置空后交接。
- 解码端：用安装器的事务回滚，或按备份恢复 unit。

## 安全

- 8.12 到 8.1 这一跳是局域网明文，API key 明文经过局域网；风险与现在局域网客户端以明文访问 8.12:11000 相同。
- 反向入口只接受 `-reverse-allow` 内的源地址，只转发名单内主机的 443，不能当作通用代理使用。
- `reverse.auth` 只有 root 可读，内容就是 sing-box 配置里已有的凭据，不新增秘密。
- 解码端名单仍是防开放代理的闸门；它只对经 anytls 认证、访问 `dedup.anytls.arpa` 的流量可见。

## 验证

- anytls-go：反向入口的单元与链路测试，覆盖名单、源地址、认证文件、SIGHUP 热加载（解析失败保留旧名单）、解码端名单头、解码端不可达或不接受时走直连、流式回复不缓冲、WebSocket 走直连、与拦截流量共用通道（同一 epoch、同一索引）；`back -domains-file` 热加载与 `/health` 头；安装脚本迁移测试（`scripts/test-systemd-dedup.py`）。
- Stargate：`dedup.sh reverse` 的事务测试（名单文件原子替换、参数不变、只发 SIGHUP），`sh manage.sh check`。
- 上线前：独立审查（deepseek）覆盖两边的改动与上线命令；通过后才交给操作者执行。
