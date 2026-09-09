package probe

import (
	"crypto/tls"
	"net/http"
	"time"

	"isp-probe/internal/dnsx"
	"isp-probe/internal/httpx"
	"isp-probe/internal/netbind"
)

// speedTransport 构造专用于带宽测速的 Transport。
//
// 与常规探测的关键差别是**禁用 HTTP/2**：HTTP/2 会把并发流复用到单条 TCP
// 连接上，受单连接拥塞控制与流量窗口限制，跑不满千兆。测速需要的是多条
// 独立 TCP 连接并行拉流。
func speedTransport(b *netbind.Binder, r *dnsx.Resolver, streams int) *http.Transport {
	tr := b.Transport()
	tr.DialContext = httpx.ResolvingDialer(b, r)
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.MaxConnsPerHost = streams * 2
	tr.MaxIdleConnsPerHost = streams * 2
	tr.DisableKeepAlives = false
	tr.ResponseHeaderTimeout = 15 * time.Second
	return tr
}
