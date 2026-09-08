// Package netbind 提供绑定到指定物理网卡的网络拨号能力。
//
// 存在的意义：本机常驻 clash verge (verge-mihomo) 且开启 TUN，它通过
// 0.0.0.0/1 + 128.0.0.0/1 分片默认路由接管全部出站流量，并劫持 UDP:53。
// 要测到真实的宽带线路质量，必须从三个层面同时绕开：
//
//  1. socket 层 —— 绑定物理接口，跳过路由表选路（本文件 + control_*.go）
//  2. DNS   层 —— 绝不使用系统解析器（见 internal/dnsx）
//  3. 代理   层 —— Transport.Proxy 显式置 nil
//
// 三者缺一不可：只做 1 而用 net.Dialer 传主机名，仍会先经系统解析器拿到
// fake-ip（198.18.0.0/16），随后连一个绑定了物理网卡却根本不可路由的地址。
package netbind

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// Binder 描述一个绑定目标：某块物理网卡。
type Binder struct {
	Name   string     // 接口名，如 "en0"，仅用于日志与展示
	Index4 uint32     // IPv4 接口索引
	Index6 uint32     // IPv6 接口索引（Windows 上与 Index4 可能不同，不可混用）
	IPv4   netip.Addr // 该网卡的 IPv4，用于 LocalAddr 双保险
	IPv6   netip.Addr
	// MAC 是该网卡的硬件地址。路由器按 MAC 绑定 WAN 口策略，
	// 测速引导需要把它展示给用户去路由器配置页填写。
	MAC string

	// BindLocalAddr 额外把源地址绑到网卡 IP 上做双保险。
	// 接口索引绑定本身已经足够，且对 DHCP 换 IP 免疫；这里作为在
	// 某些 Windows 版本或存在第三方 LSP 时的兜底手段。
	BindLocalAddr bool
}

// Control 返回可直接赋给 net.Dialer.Control / net.ListenConfig.Control 的回调。
func (b *Binder) Control() func(network, address string, c syscall.RawConn) error {
	return control(b.Index4, b.Index6)
}

// ErrHostname 表示调用方传了主机名而非 IP 字面量。
//
// 这是刻意设置的硬性防线：一旦放行主机名，net.Dialer 会先走系统解析器，
// 在开启 TUN 的机器上必然拿到 fake-ip，整个探测结果随即失去意义 ——
// 而且失败得很隐蔽（连接会成功，只是走了代理）。所有目标必须由
// internal/dnsx 自行解析后以 IP 字面量传入。
type ErrHostname struct{ Addr string }

func (e *ErrHostname) Error() string {
	return fmt.Sprintf("netbind: 目标 %q 是主机名而非 IP 字面量；"+
		"请先用 dnsx 解析，绝不能让系统解析器介入（会返回 fake-ip）", e.Addr)
}

func requireIPLiteral(address string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("netbind: 地址 %q 格式错误: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, &ErrHostname{Addr: address}
	}
	return ip, nil
}

// Dialer 构造一个绑定到本网卡的拨号器。network 用于决定 LocalAddr 的类型。
func (b *Binder) Dialer(network string, timeout time.Duration) *net.Dialer {
	d := &net.Dialer{
		Timeout:   timeout,
		Control:   b.Control(),
		KeepAlive: -1, // 探测连接短命，不需要 keepalive
	}
	if b.BindLocalAddr && b.IPv4.IsValid() {
		// Port 必须为 0，让内核自选源端口。
		switch {
		case network == "tcp" || network == "tcp4":
			d.LocalAddr = &net.TCPAddr{IP: b.IPv4.AsSlice()}
		case network == "udp" || network == "udp4":
			d.LocalAddr = &net.UDPAddr{IP: b.IPv4.AsSlice()}
		}
	}
	return d
}

// DialContext 拨号到 address，address 的 host 部分必须是 IP 字面量。
func (b *Binder) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ip, err := requireIPLiteral(address)
	if err != nil {
		return nil, err
	}
	// 显式钉死协议族，避免 Go 在 "tcp" 上创建 dual-stack socket 后
	// 只设上了其中一族的绑定选项。
	if network == "tcp" {
		if ip.Is4() {
			network = "tcp4"
		} else {
			network = "tcp6"
		}
	} else if network == "udp" {
		if ip.Is4() {
			network = "udp4"
		} else {
			network = "udp6"
		}
	}

	timeout := 8 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 && d < timeout {
			timeout = d
		}
	}
	return b.Dialer(network, timeout).DialContext(ctx, network, address)
}

// ListenConfig 用于需要 listen 的场景（如 raw ICMP socket）。
func (b *Binder) ListenConfig() *net.ListenConfig {
	return &net.ListenConfig{Control: b.Control()}
}

// Transport 构造一个绕过 clash 的 HTTP Transport。
func (b *Binder) Transport() *http.Transport {
	return &http.Transport{
		// 显式置 nil：不读 HTTP_PROXY 等环境变量。
		// Go 标准库从不读取 Windows 注册表的系统代理设置，所以这一条就够了。
		Proxy:       nil,
		DialContext: b.DialContext,
		// 必须显式打开：一旦设置了自定义 DialContext，
		// Go 会默认关掉 HTTP/2 自动协商。
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:          8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   6 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		// 测速与计时场景下避免解压开销污染读数。
		DisableCompression: true,
	}
}

// HTTPClient 构造绕过 clash 的 HTTP 客户端。
func (b *Binder) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: b.Transport(),
		Timeout:   timeout,
		// 探测请求不跟随跳转，避免重定向到未解析的主机名。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Unbound 返回一个**不做**任何绑定的拨号器，仅供 doctor 做对照实验：
// 对比 bound 与 unbound 的 RTT 和出口 IP，即可证明绑定确实生效。
func Unbound() *net.Dialer {
	return &net.Dialer{Timeout: 8 * time.Second, KeepAlive: -1}
}
