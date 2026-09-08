package probe

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"

	"isp-probe/internal/dnsx"
	"isp-probe/internal/netbind"
)

// resolvingDialer 返回一个接受主机名的拨号函数：先用 DoH 解析，再交给
// 绑定了物理网卡的拨号器。
//
// 为什么要这一层：netbind 默认拒绝主机名，逼调用方自己解析，防止流量
// 悄悄走系统解析器拿到 fake-ip。但把域名替换成 IP 再请求会带来新问题 ——
// TLS 握手会拿 IP 当 ServerName 去校验证书，而站点证书通常没有 IP SAN，
// 握手直接失败（实测 mirrors.aliyun.com、speed.cloudflare.com 均如此）。
//
// 正确的分工是：URL 始终保留域名（SNI 与 Host 头因而都正确），把「解析」
// 收敛到 dialer 里用 DoH 完成。netbind 那道防线依然在，只是这里给出了
// 一条显式的、经过 DoH 的合法通路。
func resolvingDialer(b *netbind.Binder, r *dnsx.Resolver) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		// 已经是 IP 字面量就直接连。
		if _, err := netip.ParseAddr(host); err == nil {
			return b.DialContext(ctx, network, addr)
		}
		ip, err := r.LookupOne(ctx, host)
		if err != nil {
			return nil, err
		}
		return b.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
}

// newHTTPClient 构造一个能直接使用域名 URL、且绕过 clash 的 HTTP 客户端。
func newHTTPClient(b *netbind.Binder, r *dnsx.Resolver, timeout time.Duration) *http.Client {
	tr := b.Transport()
	tr.DialContext = resolvingDialer(b, r)
	return &http.Client{Transport: tr, Timeout: timeout}
}

// speedTransport 构造专用于带宽测速的 Transport。
//
// 与常规探测的关键差别是**禁用 HTTP/2**：HTTP/2 会把并发流复用到单条 TCP
// 连接上，受单连接拥塞控制与流量窗口限制，跑不满千兆。测速需要的是多条
// 独立 TCP 连接并行拉流。
func speedTransport(b *netbind.Binder, r *dnsx.Resolver, streams int) *http.Transport {
	tr := b.Transport()
	tr.DialContext = resolvingDialer(b, r)
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.MaxConnsPerHost = streams * 2
	tr.MaxIdleConnsPerHost = streams * 2
	tr.DisableKeepAlives = false
	tr.ResponseHeaderTimeout = 15 * time.Second
	return tr
}

// newRequest 构造一个带统一 UA 的请求。URL 保留域名，由 dialer 负责解析。
func newRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ISP-Probe/1.0")
	return req, nil
}
