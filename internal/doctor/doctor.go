// Package doctor 做运行时环境自检。
//
// 设计原则：不假设环境，探测环境，并把结论如实展示给用户。
//
// 绕过 clash 这件事牵涉太多环境变量 —— TUN 有没有开、是不是真的接管了路由、
// strict-route 有没有封 53 端口、绑定到底生没生效。任何一条假设错了，探测
// 结果都会安静地变成假数据（而不是报错），这比没有数据更危险。
package doctor

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/miekg/dns"

	"isp-probe/internal/config"
	"isp-probe/internal/dnsx"
	"isp-probe/internal/iface"
	"isp-probe/internal/netbind"
)

// Level 表示一项检查结果的严重程度。
type Level string

const (
	LevelOK   Level = "ok"
	LevelInfo Level = "info"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
)

// Check 是一项检查的结果。
type Check struct {
	Name   string `json:"name"`
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	// Hint 在出问题时给出可操作的建议。
	Hint string `json:"hint,omitempty"`
}

// Report 是一次完整自检的结果。
type Report struct {
	TS     time.Time `json:"ts"`
	Iface  string    `json:"iface"`
	Checks []Check   `json:"checks"`

	// 以下字段供程序判断能力可用性，而非仅供展示。
	BindWorks       bool `json:"bind_works"`
	TUNPresent      bool `json:"tun_present"`
	DNSPort53Usable bool `json:"dns_port53_usable"`
	IPv6Usable      bool `json:"ipv6_usable"`
	ModemChannelOK  bool `json:"modem_channel_ok"`

	// unboundLocal 是未绑定连接的本地端点，作为「TUN 是否接管流量」的行为证据。
	unboundLocal netip.Addr
}

// fakeIPRange 是 mihomo 默认的 fake-ip 网段（RFC2544 基准测试网段）。
var fakeIPRange = netip.MustParsePrefix("198.18.0.0/15")

// IsFakeIP 判断一个地址是否落在 fake-ip 网段。
func IsFakeIP(ip netip.Addr) bool {
	return fakeIPRange.Contains(ip.Unmap())
}

// Run 执行完整自检。
func Run(ctx context.Context, cfg config.Config, p iface.PhysicalIface, b *netbind.Binder) *Report {
	r := &Report{TS: time.Now(), Iface: p.String()}

	r.checkIface(p)
	// 顺序有讲究：先做绑定实验拿到未绑定连接的本地端点，
	// 再据此判定 TUN 是否真的接管了流量。
	r.checkBinding(ctx, b, cfg)
	r.checkTUN(b)
	r.checkSystemResolver(ctx)
	r.checkProxyEnv()
	r.checkDoH(ctx, b)
	r.checkModemChannel(ctx, cfg, b)
	r.checkPlatform(ctx, b)

	return r
}

func (r *Report) add(c Check) { r.Checks = append(r.Checks, c) }

func (r *Report) checkIface(p iface.PhysicalIface) {
	kind := "有线"
	if p.IsWireless {
		kind = "Wi-Fi"
	}
	c := Check{
		Name:   "物理网卡识别",
		Level:  LevelOK,
		Detail: fmt.Sprintf("%s（%s），IP %s，MAC %s，网关 %s", p.Name, kind, p.IPv4, p.MAC, p.Gateway),
	}
	if p.IsWireless {
		c.Level = LevelInfo
		c.Hint = "经无线链路测速，吞吐上限受 Wi-Fi 速率制约，可能跑不满宽带带宽"
	}
	r.add(c)
}

// checkTUN 检测是否存在 TUN 接口以及它是否接管了出站流量。
//
// 「是否接管」优先采用行为证据：未绑定的连接如果本地端点不是物理网卡地址，
// 说明这条连接被 TUN 接住了。这比解析路由表更直接，且跨平台一致。
func (r *Report) checkTUN(b *netbind.Binder) {
	names, routeHijacked := detectTUN()
	r.TUNPresent = len(names) > 0

	behaviorHijacked := r.unboundLocal.IsValid() && r.unboundLocal != b.IPv4
	hijacked := routeHijacked || behaviorHijacked

	if !r.TUNPresent && !hijacked {
		r.add(Check{Name: "clash TUN", Level: LevelInfo,
			Detail: "未检测到 TUN 接口，当前无需绕过"})
		return
	}

	desc := "未命名接口"
	if len(names) > 0 {
		desc = strings.Join(names, ", ")
	}
	if !hijacked {
		r.add(Check{Name: "clash TUN", Level: LevelInfo,
			Detail: fmt.Sprintf("检测到 TUN 接口 %s，但未接管出站流量", desc)})
		return
	}

	detail := fmt.Sprintf("检测到 TUN 接口 %s，已接管出站流量", desc)
	if behaviorHijacked {
		detail += fmt.Sprintf("（未绑定的连接落在 %s 上）", r.unboundLocal)
	}
	r.add(Check{Name: "clash TUN", Level: LevelInfo, Detail: detail,
		Hint: "这正是本工具存在的前提；「接口绑定」一项已验证绕过是否生效"})
}

// checkSystemResolver 验证系统解析器是否已被劫持。
//
// 这一项不是为了使用系统解析器，恰恰相反 —— 它是向用户证明「为什么我们
// 必须自己做 DNS」的证据。
func (r *Report) checkSystemResolver(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	ips, err := net.DefaultResolver.LookupNetIP(cctx, "ip4", "www.baidu.com")
	if err != nil {
		r.add(Check{Name: "系统解析器", Level: LevelWarn,
			Detail: "系统解析器查询失败: " + err.Error(),
			Hint:   "不影响本工具运行（我们走 DoH），仅作参考"})
		return
	}
	for _, ip := range ips {
		if IsFakeIP(ip) {
			r.add(Check{Name: "系统解析器", Level: LevelInfo,
				Detail: fmt.Sprintf("返回 fake-ip %s，已被 clash 劫持", ip),
				Hint:   "本工具全程使用 DoH 自行解析，不受影响"})
			return
		}
	}
	r.add(Check{Name: "系统解析器", Level: LevelInfo,
		Detail: fmt.Sprintf("返回真实地址 %v（未被 fake-ip 劫持）", ips)})
}

// checkBinding 是最关键的一项：验证接口绑定确实让流量绕过了 TUN。
//
// 判据不用 RTT 对比（会受网络波动干扰），而用连接的本地端点地址：
// 绑定生效时本地地址是物理网卡 IP；未生效时会落在 TUN 的网段上，
// 说明连接其实被 clash 接管了。这个判据是确定性的。
func (r *Report) checkBinding(ctx context.Context, b *netbind.Binder, cfg config.Config) {
	if len(cfg.Probe.Anchors) == 0 {
		r.add(Check{Name: "接口绑定", Level: LevelWarn, Detail: "未配置锚点，无法验证"})
		return
	}
	target := cfg.Probe.Anchors[0]

	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	boundStart := time.Now()
	boundConn, boundErr := b.DialContext(cctx, "tcp4", target)
	boundRTT := time.Since(boundStart)
	var boundLocal netip.Addr
	if boundErr == nil {
		if ap, err := netip.ParseAddrPort(boundConn.LocalAddr().String()); err == nil {
			boundLocal = ap.Addr()
		}
		boundConn.Close()
	}

	unboundStart := time.Now()
	unboundConn, unboundErr := netbind.Unbound().DialContext(cctx, "tcp4", target)
	unboundRTT := time.Since(unboundStart)
	var unboundLocal netip.Addr
	if unboundErr == nil {
		if ap, err := netip.ParseAddrPort(unboundConn.LocalAddr().String()); err == nil {
			unboundLocal = ap.Addr()
		}
		unboundConn.Close()
	}
	r.unboundLocal = unboundLocal

	if boundErr != nil {
		r.add(Check{Name: "接口绑定", Level: LevelFail,
			Detail: fmt.Sprintf("绑定 %s 后无法连接 %s: %v", b.Name, target, boundErr),
			Hint:   "检查网卡是否正常，或在配置里用 iface.name 指定正确的网卡"})
		return
	}

	bindOK := boundLocal.IsValid() && boundLocal == b.IPv4
	r.BindWorks = bindOK

	detail := fmt.Sprintf("绑定后本地端点 %s (%.1fms)", boundLocal, ms(boundRTT))
	if unboundErr == nil {
		detail += fmt.Sprintf("；未绑定时 %s (%.1fms)", unboundLocal, ms(unboundRTT))
	}

	switch {
	case !bindOK:
		r.add(Check{Name: "接口绑定", Level: LevelFail,
			Detail: detail + fmt.Sprintf("；期望本地端点为网卡地址 %s", b.IPv4),
			Hint:   "绑定未生效，测得的数据不可信"})
	case unboundErr == nil && unboundLocal.IsValid() && unboundLocal != b.IPv4:
		// 理想情况：绑定后走物理网卡，不绑定则落到 TUN，对比鲜明。
		r.add(Check{Name: "接口绑定", Level: LevelOK,
			Detail: detail + " —— 绑定生效，已绕过 TUN",
			Hint:   "未绑定时的低延迟是假象：clash 本地接受连接即完成握手，并未真正出网"})
	default:
		r.add(Check{Name: "接口绑定", Level: LevelOK, Detail: detail + " —— 绑定生效"})
	}
}

func (r *Report) checkProxyEnv() {
	var found []string
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if v := os.Getenv(k); v != "" {
			found = append(found, k+"="+v)
		}
	}
	if len(found) == 0 {
		r.add(Check{Name: "代理环境变量", Level: LevelOK, Detail: "未设置"})
		return
	}
	r.add(Check{Name: "代理环境变量", Level: LevelInfo,
		Detail: "检测到 " + strings.Join(found, ", "),
		Hint:   "本工具已显式忽略（Transport.Proxy = nil），不影响探测"})
}

func (r *Report) checkDoH(ctx context.Context, b *netbind.Binder) {
	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	res := dnsx.NewResolver(b, nil)
	st := time.Now()
	addrs, err := res.LookupA(cctx, "www.baidu.com")
	if err != nil {
		r.add(Check{Name: "DoH 解析", Level: LevelFail,
			Detail: "失败: " + err.Error(),
			Hint:   "无法解析域名，测速与出口归属查询将不可用"})
		return
	}
	for _, ip := range addrs {
		if IsFakeIP(ip) {
			r.add(Check{Name: "DoH 解析", Level: LevelFail,
				Detail: fmt.Sprintf("返回了 fake-ip %s，说明 DoH 请求仍被 clash 接管", ip),
				Hint:   "接口绑定可能未对 HTTPS 生效"})
			return
		}
	}
	r.add(Check{Name: "DoH 解析", Level: LevelOK,
		Detail: fmt.Sprintf("%v (%.0fms)，未被 fake-ip 污染", addrs, ms(time.Since(st)))})
}

// checkModemChannel 验证零配置分线通道是否可用。
func (r *Report) checkModemChannel(ctx context.Context, cfg config.Config, b *netbind.Binder) {
	mp := dnsx.NewModemProbe(b, cfg.Probe.MinGap, cfg.Probe.Timeout)
	allOK := true

	for i := range cfg.Links {
		l := &cfg.Links[i]
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)

		alive, aliveRTT := mp.Alive(cctx, l.ModemAddr(), l.ModemPorts)
		if !alive {
			allOK = false
			r.add(Check{Name: fmt.Sprintf("分线通道 · %s", l.Name), Level: LevelFail,
				Detail: fmt.Sprintf("光猫 %s 不可达", l.ModemAddr()),
				Hint:   "检查光猫是否在线、IP 是否正确（配置项 links[].modem_ip）"})
			cancel()
			continue
		}

		q := mp.Query(cctx, l.ModemAddr(), cfg.Probe.CacheDomain)
		cancel()

		if !q.OK() {
			allOK = false
			r.add(Check{Name: fmt.Sprintf("分线通道 · %s", l.Name), Level: LevelFail,
				Detail: fmt.Sprintf("光猫 %s 在线（%.0fms）但 DNS 查询失败: %v", l.ModemAddr(), ms(aliveRTT), q.Err),
				Hint:   "该光猫可能未开启 DNS 中继，或这条线路当前不通"})
			continue
		}
		r.add(Check{Name: fmt.Sprintf("分线通道 · %s", l.Name), Level: LevelOK,
			Detail: fmt.Sprintf("经光猫 %s 解析得 %v (%.0fms)", l.ModemAddr(), q.Addrs, ms(q.RTT))})
	}
	r.ModemChannelOK = allOK
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// HasFailure 表示存在导致探测结果不可信的问题。
func (r *Report) HasFailure() bool {
	for _, c := range r.Checks {
		if c.Level == LevelFail {
			return true
		}
	}
	return false
}

// checkPlatform 实测 UDP:53 与 IPv6 出站能力。
//
// 这两项在 Windows 上会被 mihomo 的 strict-route 用 WFP 在内核层封掉：
// 远端 53 端口的连接一律 BLOCK（防 DNS 泄漏），TUN 无 IPv6 地址时全部
// IPv6 出站也被 BLOCK。macOS 上则没有这层拦截。
//
// 与其按平台假设，不如直接实测 —— 结果决定了两件事：
// 明文 DNS 能否作为 DoH 的备选，以及分线通道（走光猫 UDP:53）是否可用。
func (r *Report) checkPlatform(ctx context.Context, b *netbind.Binder) {
	r.checkDNSPort53(ctx, b)
	r.checkIPv6(ctx, b)
}

func (r *Report) checkDNSPort53(ctx context.Context, b *netbind.Binder) {
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn("www.baidu.com"), dns.TypeA)
	msg.RecursionDesired = true

	c := &dns.Client{Net: "udp4", Dialer: b.Dialer("udp4", 5*time.Second), Timeout: 5 * time.Second}
	reply, _, err := c.ExchangeContext(cctx, msg, "223.5.5.5:53")

	if err != nil {
		if blocked, hint := explainBlockErr(err); blocked {
			r.DNSPort53Usable = false
			r.add(Check{Name: "明文 DNS (UDP:53)", Level: LevelWarn,
				Detail: "到公网 DNS 的 53 端口被拦截: " + err.Error(),
				Hint:   hint + "。本工具主用 DoH 不受影响，但分线通道依赖光猫的 UDP:53，会一并失效"})
			return
		}
		r.DNSPort53Usable = false
		r.add(Check{Name: "明文 DNS (UDP:53)", Level: LevelWarn,
			Detail: "查询失败: " + err.Error(),
			Hint:   "本工具主用 DoH，此项不影响公网域名解析"})
		return
	}

	// 拿到响应还不够，要确认没被 fake-ip 污染。
	for _, rr := range reply.Answer {
		if a, ok := rr.(*dns.A); ok {
			if ip, ok := netip.AddrFromSlice(a.A.To4()); ok && IsFakeIP(ip) {
				r.DNSPort53Usable = false
				r.add(Check{Name: "明文 DNS (UDP:53)", Level: LevelWarn,
					Detail: fmt.Sprintf("返回 fake-ip %s，查询仍被 clash 接管", ip),
					Hint:   "本工具主用 DoH，不受影响"})
				return
			}
		}
	}
	r.DNSPort53Usable = true
	r.add(Check{Name: "明文 DNS (UDP:53)", Level: LevelOK,
		Detail: "绑定网卡后可直达公网 DNS，未被拦截也未被 fake-ip 污染"})
}

func (r *Report) checkIPv6(ctx context.Context, b *netbind.Binder) {
	if !b.IPv6.IsValid() {
		r.IPv6Usable = false
		r.add(Check{Name: "IPv6", Level: LevelInfo,
			Detail: "本机网卡无全局 IPv6 地址，跳过 IPv6 探测"})
		return
	}

	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	// 阿里公共 DNS 的 IPv6 地址。
	conn, err := b.DialContext(cctx, "tcp6", "[2400:3200::1]:443")
	if err != nil {
		if blocked, hint := explainBlockErr(err); blocked {
			r.IPv6Usable = false
			r.add(Check{Name: "IPv6", Level: LevelWarn,
				Detail: "IPv6 出站被拦截: " + err.Error(), Hint: hint})
			return
		}
		r.IPv6Usable = false
		r.add(Check{Name: "IPv6", Level: LevelInfo,
			Detail: "IPv6 连接失败: " + err.Error(),
			Hint:   "可能是上游未提供 IPv6；不影响 IPv4 探测"})
		return
	}
	conn.Close()
	r.IPv6Usable = true
	r.add(Check{Name: "IPv6", Level: LevelOK, Detail: "IPv6 出站可用"})
}
