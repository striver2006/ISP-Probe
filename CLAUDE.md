# ISP-Probe 项目规则

## 项目性质

家庭双线宽带（电信 + 移动 1:1 分流）接入点监测工具。运行主机常驻 clash verge (verge-mihomo) 且开启 TUN，**所有网络探测必须绕过它**，否则测到的是代理链路而非真实宽带。

## 不可违背的约束

### 1. 绝不使用系统 DNS 解析器

`net.Resolver` / `net.LookupHost` / `net.LookupIP` 在本机会返回 fake-ip（`198.18.0.0/15`）。所有域名解析必须经 `internal/dnsx` 的 DoH。

`netbind.Binder.DialContext` 会**拒绝主机名**，这是刻意的防线，不要为了图方便绕过它。需要用域名的场景（HTTP 请求）走 `probe.resolvingDialer`，它在 dialer 层用 DoH 解析。

### 2. 所有出站 socket 必须绑定物理网卡

经 `netbind.Binder` 创建连接。直接用 `net.Dial` 或默认 `http.Client` 的代码都是错的 —— 它们会落到 TUN 上，且**不会报错**，只会安静地返回假数据。

### 3. 不要把无法归属的指标伪装成分线数据

TCP 连接走哪条 WAN 由路由器负载均衡决定，主机指定不了。因此锚点延迟、ICMP 丢包这类指标**不能**挂到某条线路上，它们记在 `probe.CombinedLinkID`（`_combined`）下。

只有经指定光猫的 DNS 查询是确定性走指定 WAN 的，分线指标只能基于它。

### 4. 光猫 DNS 有速率限制

实测快速连发 5 次即全部超时，而线路本身完好。`dnsx.ModemProbe` 内置最小间隔限流，改动探测频率时务必保持 `probe.min_gap`，否则会把自己造成的限速误报成线路故障。

### 5. 服务模式下 cwd 不由我们决定

launchd / Windows SCM 启动进程时 cwd 通常是 `/` 或 `C:\Windows\System32`。所有相对路径必须经 `apppath.Paths` / `config.Anchor` 锚定到配置文件所在目录，**不要新增任何直接吃相对路径的文件访问**。

`-c` 不指定时的查找顺序是 cwd → 可执行文件目录，两者都试。这个顺序不能反：`go run` 时可执行文件在 `/tmp/go-build.../`，只有 cwd 优先才找得到仓库里的 config.yaml。

### 6. 只有 install 需要网卡探测

`service status` / `stop` / `uninstall` / `logs` 不加载配置、不调 `iface.Detect` —— 它们恰恰是出问题时要用的命令，不能依赖可能已经失败的那两步。给 service 子命令组加新命令时保持这条。

### 7. 退出路径上的写入不能跟着 ctx 取消

`RunOnce` 的落库阶段用 `context.WithoutCancel` 派生的独立 context。探测该被取消，写入不该 —— 沿用已取消的 ctx 会让本轮采到的数据全部以 `context canceled` 失败。超时预算是递增的，不能倒挂：落库 5s < `serveLoop` 等 `Monitor.Done()` 8s < Windows SCM WaitHint 15/20s。

### 8. 通知的开关下沉到构造，不要在状态机里判

`updateStatus` 里曾经写着 `if m.notifier != nil && m.cfg.Notify.Desktop`，于是关掉桌面通知（Windows 服务模式下的标准建议）会把 IM 通知一起关掉。渠道启用与否在 `buildNotifier` 里决定，状态机只管把事件交出去。

通知发送必须异步。`Notify` 是在 `RunOnce` 的落库循环里同步调用的，一次 8 秒超时的 HTTP 请求会拖住整轮探测。`notify.Webhook` 只入队，队列满了宁可丢弃也不阻塞。

### 9. webhook URL 是密钥

URL 里的 key/token 等同于发消息的权限。它不能出现在日志里，也不能出现在 `/api/notify` 的响应里（面板可能开放在局域网上）—— 统一走 `notify.MaskURL`。密钥放 `config.local.yaml`，那个文件不进 git。

对端返回 **HTTP 200 不代表发送成功**：企微/钉钉/飞书在 key 失效时照样返回 200，错误码在 body 的 `errcode` / `code` 里。只看状态码会把彻底发不出去的渠道当成正常。

### 10. 监听地址不等于可连接地址

`0.0.0.0` / `::` / 空 host 是「监听所有接口」的写法。要从本机连它必须先过 `dialTarget` 归一化成 `127.0.0.1` —— macOS 上连 `0.0.0.0` 碰巧能通，Windows 上直接 `WSAEADDRNOTAVAIL`，于是单实例保护静默失效，能起两个进程抢同一个数据库。任何新增的「连自己面板」的代码都要走它。

面板鉴权对环回永远放行：本机 CLI、`service install` 的存活自检、单实例检测都在打自己的 `/api/status`。

## 代码约定

- 环境相关的值（IP、URL、MAC、策略名）一律进 `config.yaml`，代码里不硬编码
- 注释写「为什么」，尤其是那些看起来可以简化、实则踩过坑的地方（如禁用 HTTP/2、上传必须设 Content-Length）
- 平台相关代码用 build tag 分文件，共享逻辑抽接口
- 纯 Go 依赖，保持 `CGO_ENABLED=0` 可交叉编译

## 提交前自检

```bash
go vet ./... && GOOS=windows go vet ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
go test ./...
./isp-probe doctor    # 必须全绿
```

## Git 约定

- Conventional Commits，subject 用中文
- 末尾附 `Edit by CZB`
- 逐个 `git add <path>`，禁止 `git add -A` / `git add .`
- 代码变更同步更新 `docs/CHANGELOG.md`

## 文档

- `README.md` — 面向使用者
- `docs/CHANGELOG.md` — 变更记录
- `CLAUDE.md` — 本文件，面向后续开发
