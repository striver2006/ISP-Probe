# 贡献指南

## 提交前必须通过

```bash
go vet ./... && GOOS=windows go vet ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
go test ./...
./isp-probe doctor    # 必须全绿
```

CI 会跑同一套检查（见 [.github/workflows/ci.yml](.github/workflows/ci.yml)）。`doctor` 依赖真实网络环境，只能本地跑。

## 几条不可违背的约束

这个项目的运行主机常驻 clash verge 且开启 TUN。下面几条不是风格偏好，是**违反了不会报错、只会安静地产出假数据**的地方。改动前请先读 [CLAUDE.md](CLAUDE.md) 的完整版本。

1. **绝不使用系统 DNS 解析器**。`net.Resolver` / `net.LookupHost` 在本机返回 fake-ip（`198.18.0.0/15`）。所有域名解析走 `internal/dnsx` 的 DoH。
2. **所有出站 socket 必须绑定物理网卡**，经 `netbind.Binder` 创建。直接用 `net.Dial` 或默认 `http.Client` 的代码会落到 TUN 上且不报错。
3. **不要把无法归属的指标伪装成分线数据**。TCP 走哪条 WAN 由路由器决定，锚点延迟这类指标记在 `_combined` 下。
4. **光猫 DNS 有速率限制**。实测连发 5 次即全部超时而线路完好，改动探测频率时务必保持 `probe.min_gap`。
5. **服务模式下 cwd 不由我们决定**。所有相对路径经 `apppath.Paths` / `config.Anchor` 锚定到配置文件所在目录。
6. **退出路径上的写入不能跟着 ctx 取消**。落库用 `context.WithoutCancel` 派生的独立 context。

## 代码约定

- 环境相关的值（IP、URL、MAC、策略名）一律进 `config.yaml`，代码里不硬编码
- **注释写「为什么」**，尤其是那些看起来可以简化、实则踩过坑的地方。这个仓库里大量注释记录的是失败过的尝试，删掉它们等于让下一个人重新踩一遍
- 平台相关代码用 build tag 分文件，共享逻辑抽接口；新增跨平台能力必须成对提供 darwin + windows 两份实现，否则 `GOOS=windows go vet ./...` 会挂
- 纯 Go 依赖，保持 `CGO_ENABLED=0` 可交叉编译。这是选 `modernc.org/sqlite` 而非 `mattn/go-sqlite3` 的全部理由
- 测试用标准库 `testing`，表驱动，断言消息用中文；每个测试函数上方说明**这条不变量为什么重要**

## 提交信息

Conventional Commits，subject 用中文：

```
<type>(<scope>): <做了什么 + 为什么>
```

类型：`feat` `fix` `docs` `style` `refactor` `perf` `test` `chore` `build` `ci`。多文件多目的的变更在 body 中分点说明。

## 报告问题

请附上 `./isp-probe doctor` 的完整输出 —— 它包含判断问题所需的绝大部分信息（TUN 绕过是否生效、分线通道是否可用、DNS 是否被污染）。

**输出里含内网地址与网卡 MAC，贴之前先自行脱敏。**

## 文档

代码变更需同步更新：

- [README.md](README.md) / [README.en.md](README.en.md) — 面向使用者，两份都要改
- [docs/CHANGELOG.md](docs/CHANGELOG.md) — 变更记录，写清动机与取舍而不是流水账
- [CLAUDE.md](CLAUDE.md) — 面向后续开发者的约束说明
