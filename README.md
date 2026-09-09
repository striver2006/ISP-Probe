<div align="center">

# 📡 ISP 探针

<p>

**双线宽带接入点监测 · ISP Probe**

家庭双线宽带（电信 + 移动 1:1 分流）分线连通性监控与引导式测速工具

</p>

[![CI](https://github.com/striver2006/ISP-Probe/actions/workflows/ci.yml/badge.svg)](https://github.com/striver2006/ISP-Probe/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-macOS-lightgrey?logo=apple)](#平台支持)
[![Platform](https://img.shields.io/badge/Platform-Windows-blue?logo=windows)](#平台支持)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)
[![CGO](https://img.shields.io/badge/CGO-disabled-success)](#几个刻意的设计取舍)

简体中文 | [English](README.en.md)

</div>

---

全程绕过本机 clash verge 的 TUN 与 DNS 劫持 —— 否则测到的是代理链路，不是宽带线路。

## 解决什么问题

两条宽带分流合并使用时，某一条静默失效不会有任何感知：网还能上，只是带宽悄悄减半。本工具持续盯住每条线，掉线时立刻弹出系统通知。

## 快速开始

```bash
go build -o isp-probe ./cmd/isp-probe

./isp-probe doctor   # 先自检：确认绕过生效、分线通道可用
./isp-probe serve    # 常驻监测 + 面板 http://127.0.0.1:8686
```

想让它一直在后台跑、开机自启：

```bash
./isp-probe service install    # 装成随登录自启的服务（见「作为服务运行」）
```

## 命令

按需运行，前台执行，Ctrl-C 退出：

| 命令 | 说明 |
|---|---|
| `doctor` | 环境自检。验证 TUN 绕过是否生效、分线通道是否可用、DNS 有没有被 fake-ip 污染 |
| `probe` | 执行一次分线连通性探测并打印结果 |
| `speed` | 对当前出口所在的线路做一次上下行测速 |
| `serve` | 常驻运行，定时探测 + Web 面板 |

后台服务：

| 命令 | 说明 |
|---|---|
| `service install` | 安装为随登录自启的服务并启动 |
| `service uninstall` | 停止并卸载 |
| `service start` / `stop` | 启停已安装的服务（`stop` 不卸载，下次登录仍会自启） |
| `service status` | 查看安装与运行状态 |
| `service logs [-n N]` | 打印服务日志尾部 |

通知：

| 命令 | 说明 |
|---|---|
| `notify test` | 向已配置的 IM 渠道各发一条测试消息（`--name` 只测其中一个） |

通用选项：`-v`（调试日志）、`-c <配置文件>`。`-c` 不指定时依次在**当前目录**、**可执行文件所在目录**查找 `config.yaml` —— 前者保证仓库里直接跑的既有习惯不变，后者让整个目录拷到哪儿都能用。

## 作为服务运行

```bash
./isp-probe service install    # 安装 + 启动 + 自动验证面板是否真的起来了
./isp-probe service status
./isp-probe service logs -n 50
```

`install` 默认先跑一遍 `doctor`：TUN 绕过没生效时程序不会报错，只会安静地产出假数据 —— 一个后台常驻、无人盯着的服务产出假数据，比前台跑危险得多。确实要跳过就加 `--skip-doctor`。其它选项：`--no-start`（只装不启动）、`--force`（已安装时覆盖）、`--log-dir`。

**配置、数据库、日志都锚定在 config.yaml 所在目录**，整个文件夹搬走照样能用。服务定义里记的是安装时解析出的绝对路径，所以搬完要重新 `install`。

**日志**：服务模式写 `logs/isp-probe.log`（默认 8MB 轮转、保留 3 个，见 config.yaml 的 `log` 段）。前台运行仍然只输出到 stderr，与服务化之前没有区别。同目录下的 `launchd.out.log` / `launchd.err.log` 只兜底捕获 Go runtime panic 这类绕过日志系统的输出，正常情况下应该是空的。

**同时只能跑一个**。服务在跑的时候再执行 `serve`，会拿到写明占用者是谁的提示而不是一句 `address already in use`。`probe` / `speed` 不受影响 —— 服务跑着的时候，随手看一眼这件事不该被挡住。

### macOS

装成 **LaunchAgent**（`~/Library/LaunchAgents/com.czb.isp-probe.plist`），不需要 sudo。跑在用户的图形会话里，所以桌面通知能正常弹出；代价是**登录之后**才启动，不是严格意义的开机即启。崩溃会自动重启（最小间隔 10 秒），正常退出则不会 —— 这样 `service stop` 才不会被立刻拉回来。

别从 SSH 会话里安装：作业可能落到错误的域，结果是桌面通知静默失效。`install` 会检测并拦截，确实需要就加 `--force`。

### Windows

装成 Windows 服务（服务名 `ISPProbe`），**需要管理员权限**（`status` 除外）。启动类型是「自动（延迟启动）」：开机瞬间网络栈和物理网卡还没就绪，`iface.Detect` 会失败或选错卡，对每分钟一次的探测来说晚几十秒毫无损失。崩溃后按 5s / 30s / 60s 三级自动重启。

两个已知限制：

- **桌面通知收不到**。Windows 服务跑在 session 0，与用户桌面隔离。装完会提示你把 `notify.desktop` 改成 `false` —— 配上 [IM 通知](#im-通知)，掉线消息就能推到手机上，这正是它要解决的场景。
- 服务以 LocalSystem 身份创建数据库文件，普通用户跑 `probe` 可能因权限不足写不进去。安装目录建议放 `C:\ISP-Probe\` 这类用户可写的位置，而不是 `C:\Program Files`。

## 两种能力，配置成本不同

### 连通性监控 — 零配置

向指定光猫的 LAN IP 发 DNS 查询，包必然经路由器对应的 WAN 口出去，由该运营商的递归 DNS 应答。这条通道不依赖路由器的任何分流配置。

```
经 192.168.10.1（电信光猫）解析 www.baidu.com → 180.101.51.73   电信节点
经 192.168.1.1 （移动光猫）解析 www.baidu.com → 223.109.82.16   移动节点
```

两条线同时监控，掉线即告警 —— 桌面通知，以及推到手机的 [IM 通知](#im-通知)。

### 分线测速 — 需要一步手动操作

路由器按 MAC 绑定 WAN 口策略，一个 MAC 同时只能走一条线，所以**一次测一条**：

1. 面板点「开始测速」，程序通过出口 IP 归属识别当前走的是哪条线
2. 测完这条后，面板给出可点击的路由器链接与要改的 MAC（附一键复制）
3. 你在路由器上改绑到另一条线
4. 程序每 5 秒查一次出口归属，**检测到切换后自动开始测第二条**

## 它是怎么绕过 clash 的

本机实测的三层措施，缺一不可：

| 层 | 做法 | 不做会怎样 |
|---|---|---|
| socket | `IP_BOUND_IF`(macOS) / `IP_UNICAST_IF`(Windows) 绑定物理网卡 | 连接落到 TUN 上，测的是代理延迟 |
| DNS | 只用 DoH，绝不碰系统解析器 | 拿到 fake-ip `198.18.x.x` |
| 代理 | `Transport.Proxy = nil` | 走系统代理 |

绑定生效与否有确定性判据 —— 看连接的本地端点：

```
绑定后:  本地 192.168.31.102 (物理网卡)   12ms   真的出网了
未绑定:  本地 198.18.0.1     (clash TUN)  0.3ms  clash 本地接受连接即握手成功，并未出网
```

未绑定时那个 0.3ms 正是危险所在：它不报错，只是安静地给出假数据。`doctor` 每次都会验证这一点。

## 配置

见 `config.yaml`，所有环境相关的值都在里面，代码中不含硬编码地址：

- `links[].modem_ip` — 各线路光猫的 LAN IP，分线探测的入口
- `links[].expect_isp` — 出口归属校验关键字，用于识别当前走的是哪条线
- `links[].wan_label` — 引导文案里让用户在路由器上选的策略名
- `router.admin_url` — 面板上可点击的路由器管理页链接
- `probe.min_gap` — 同一光猫两次查询的最小间隔

**密钥不要写进 `config.yaml`** —— 它随仓库提交。同目录下放一个 `config.local.yaml`（已在 `.gitignore` 里），只写要覆盖的字段，加载时会叠加到主配置上：

```yaml
# config.local.yaml —— 不提交
web:
  token: "一串随便什么"
notify:
  webhooks:
    - name: "企微告警群"
      kind: wecom
      url: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=真实key"
      enabled: true
```

旁路文件只在**主配置文件所在目录**查找，所以服务模式下天然可用。注意**列表是整体替换而不是追加**：旁路里写了 `webhooks` 就是全量覆盖主配置里那份。文件不存在很正常；存在却解析失败会直接报错 —— 静默忽略会让你以为密钥已经生效，实际一条告警都发不出去。

## IM 通知

掉线时把消息推到手机上。这是给**服务模式**准备的：Windows 服务跑在 session 0，桌面通知根本送不到你面前，而「一条线静默失效」本来就是无人盯着的场景。

支持企业微信、钉钉、飞书群机器人，以及通用自定义 webhook。配置写在 `config.local.yaml`（见上），配完验证一下：

```bash
./isp-probe notify test          # 或在面板的「通知渠道」卡片点「测试」
```

各平台的机器人地址都从群设置里拿（群设置 → 群机器人 → 添加）。以飞书为例：

```yaml
# config.local.yaml
notify:
  webhooks:
    - name: "飞书告警群"
      kind: feishu
      url: "https://open.feishu.cn/open-apis/bot/v2/hook/xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
      secret: ""        # 安全设置选「签名校验」时填，选关键词/IP 白名单则留空
      enabled: true
```

添加机器人时飞书会要求至少选一种安全设置：

- **签名校验** — 把它给的密钥填进 `secret`。
- **自定义关键词** — 留空 `secret`，关键词填 `ISP探针` 或 `线路`；本工具发出的正文形如 `ISP探针 · 线路故障 / 电信 线路故障：…`，都能匹配上。
- **IP 白名单** — 填你家宽带的公网 IP。注意家宽 IP 会变，变了通知就静默失败，不推荐。

钉钉同理，选「加签」时把 `SEC` 开头的那串填进 `secret`。企业微信不需要 `secret`，密钥就在 URL 的 `key` 参数里。自定义 webhook 用 `body_template` 拼请求体，可用字段 `.Kind` `.LinkID` `.Message` `.Time`。

三家在**密钥失效、机器人被移出群、签名不匹配**时都返回 HTTP 200，错误码藏在响应体里，所以 `notify test` 会把 `errcode` 一起打出来 —— 看到 ✓ 才是真的发出去了。

**告警节奏**：线路连续失败达到 `probe.fail_threshold` 次时发一条 down；持续不恢复则在第 5、15、30、60 分钟各提醒一次，之后每 60 分钟一次，消息带上已持续时长；恢复时发一条 up。不想被反复打扰就把 `notify.repeat_alert` 设成 `false`。重复提醒只发通知、不写事件表，所以面板的「状态变更」列表里一次故障始终只有 down + up 两条。

**一个明确的边界**：两条线**同时**中断时，通知发不出去，也不会在恢复后补发。本功能针对的是「一条线静默失效、另一条还在正常工作」—— 那也正是这个工具存在的理由。全断的时候你自己会立刻发现，不需要程序告诉你。

## 让局域网访问面板

默认只监听 `127.0.0.1`。想从手机或另一台机器上看，改 `config.yaml`：

```yaml
web:
  listen: "0.0.0.0:8686"
```

**同时务必设置 `web.token`**（放 `config.local.yaml`）。不设的话，任何连上同一个 WiFi 的设备 —— 包括客人的手机和 IoT 设备 —— 都能看到你的内网拓扑、光猫地址、网卡 MAC，还能触发一次跑满带宽的测速。`doctor` 会在这种配置下告警。

令牌只对来自其它设备的请求生效，本机访问永远放行（否则 CLI 和单实例检测会被自己的鉴权挡住）。浏览器首次访问会提示输入，之后记在 localStorage 里。

还要在系统防火墙上放行入站：macOS 首次会弹窗询问，选「允许」；Windows 需要管理员执行 `netsh advfirewall firewall add rule name="ISP-Probe" dir=in action=allow protocol=TCP localport=8686`。`doctor` 会按平台给出对应提示。

## 几个刻意的设计取舍

**光猫 DNS 限速必须防**。实测快速连发 5 次查询会让光猫全部超时，而线路本身完好。故障判定因此分三级：DNS 超时 → 先 ping 光猫排除本地故障 → 降速重试排除限速 → 才判定线路故障。

**锚点延迟不归属到任何一条线**。TCP 连接走哪条 WAN 由路由器的负载均衡决定，主机指定不了。这个指标记在 `_combined` 伪线路下，不冒充分线数据。

**不用 ICMP 测丢包**。同上，ICMP ping 公网目标也没法指定走哪条线。分线质量用经各自光猫的 DNS 探测成功率衡量 —— 那才是确定性经过指定 WAN 的。

**交叉验证分线是否真的生效**。两条线解析同一域名，正常会返回各自运营商的节点。若结果完全相同，说明流量没被分到不同 WAN，面板会标出 —— 此时「两条线都正常」是不可信的。

## 平台支持

macOS 与 Windows，纯 Go 依赖，`CGO_ENABLED=0` 直接交叉编译：

```bash
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o isp-probe     ./cmd/isp-probe
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -o isp-probe     ./cmd/isp-probe
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o isp-probe.exe ./cmd/isp-probe
```

系统版本下限跟随 Go 工具链本身的支持范围（见 [go.mod](go.mod) 里的 Go 版本），项目代码不额外依赖更新的系统 API。刻意**不提供 Linux 构建** —— 服务管理、网卡探测、路由表解析都是分平台实现的，留个能编译但跑不了的 stub 只会制造假象。

Windows 注意：mihomo 开启 `strict-route` 时会用 WFP 在内核层封掉所有非 TUN 接口的 53 端口，分线通道会失效（`doctor` 会检测并提示）。Clash Verge Rev 默认关闭该选项。

## 参与贡献

见 [CONTRIBUTING.md](CONTRIBUTING.md)。简而言之：`go vet ./... && GOOS=windows go vet ./... && go test ./...` 要过，`./isp-probe doctor` 要全绿。

提 issue 时请附上 `doctor` 的输出（**注意先脱敏内网地址与 MAC**）。

## 变更记录

见 [docs/CHANGELOG.md](docs/CHANGELOG.md)。

## 许可证

[MIT](LICENSE)
