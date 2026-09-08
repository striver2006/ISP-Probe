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
