// Package dnsx 提供绕过 clash DNS 劫持的域名解析，以及经指定光猫的分线 DNS 探测。
//
// 为什么不能用系统解析器：clash TUN 劫持了所有 UDP:53，本机实测
// `dig @223.5.5.5` 与 `dig @114.114.114.114` 都返回 fake-ip 198.18.0.210。
// Go 的 net.Resolver 无论 PreferGo 与否都会中招（纯 Go 解析器的 nameserver
// 列表同样来自系统网卡配置，而 TUN 网卡带 DNS 且有网关，过滤不掉）。
//
// 因此本包只走两条路：
//   - DoH over HTTPS/443：解析公网域名。443 端口不受 TUN 的 53 端口规则影响，
//     且服务器地址本身就是 IP 字面量，不存在先有鸡还是先有蛋的问题。
//   - 光猫 UDP:53 直查：分线探测专用，见 modem.go。
package dnsx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"

	"isp-probe/internal/netbind"
)

// DoHServer 是一个 DoH 端点。URL 的 host 必须是 IP 字面量 ——
// 否则解析它自己又要用到 DNS，陷入循环。
type DoHServer struct {
	Name string
	URL  string
}

// DefaultDoHServers 是国内可用、且支持以 IP 直接访问（证书含 IP SAN）的端点。
var DefaultDoHServers = []DoHServer{
	{Name: "阿里", URL: "https://223.5.5.5/dns-query"},
	{Name: "阿里备", URL: "https://223.6.6.6/dns-query"},
	{Name: "腾讯", URL: "https://120.53.53.53/dns-query"},
}

type cacheEntry struct {
	addrs  []netip.Addr
	expire time.Time
}

// Resolver 是绑定了物理网卡的 DoH 解析器。
type Resolver struct {
	servers []DoHServer
	client  *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
}

func NewResolver(b *netbind.Binder, servers []DoHServer) *Resolver {
	if len(servers) == 0 {
		servers = DefaultDoHServers
	}
	return &Resolver{
		servers: servers,
		client:  b.HTTPClient(10 * time.Second),
		cache:   make(map[string]cacheEntry),
	}
}

// LookupA 解析域名的 A 记录，依次尝试各个 DoH 服务器直到成功。
func (r *Resolver) LookupA(ctx context.Context, name string) ([]netip.Addr, error) {
	fqdn := dns.Fqdn(name)

	r.mu.Lock()
	if e, ok := r.cache[fqdn]; ok && time.Now().Before(e.expire) {
		addrs := e.addrs
		r.mu.Unlock()
		return addrs, nil
	}
	r.mu.Unlock()

	var lastErr error
	for _, s := range r.servers {
		addrs, ttl, err := r.queryOne(ctx, s, fqdn)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", s.Name, err)
			continue
		}
		if len(addrs) == 0 {
			lastErr = fmt.Errorf("%s: 无 A 记录", s.Name)
			continue
		}
		// TTL 下限 30s、上限 10min，避免过于频繁地重查或长期使用陈旧结果。
		if ttl < 30 {
			ttl = 30
		} else if ttl > 600 {
			ttl = 600
		}
		r.mu.Lock()
		r.cache[fqdn] = cacheEntry{addrs: addrs, expire: time.Now().Add(time.Duration(ttl) * time.Second)}
		r.mu.Unlock()
		return addrs, nil
	}
	return nil, fmt.Errorf("DoH 解析 %s 失败: %w", name, lastErr)
}

// LookupOne 返回首个 A 记录，是最常用的形式。
func (r *Resolver) LookupOne(ctx context.Context, name string) (netip.Addr, error) {
	addrs, err := r.LookupA(ctx, name)
	if err != nil {
		return netip.Addr{}, err
	}
	return addrs[0], nil
}

// queryOne 向单个 DoH 端点发起 RFC 8484 wireformat 查询。
func (r *Resolver) queryOne(ctx context.Context, s DoHServer, fqdn string) ([]netip.Addr, uint32, error) {
	m := new(dns.Msg)
	m.SetQuestion(fqdn, dns.TypeA)
	m.RecursionDesired = true

	wire, err := m.Pack()
	if err != nil {
		return nil, 0, fmt.Errorf("打包查询失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(wire))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// DNS 响应很小，限制读取上限防止异常响应撑爆内存。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, 0, err
	}

	reply := new(dns.Msg)
	if err := reply.Unpack(body); err != nil {
		return nil, 0, fmt.Errorf("解析响应失败: %w", err)
	}
	if reply.Rcode != dns.RcodeSuccess {
		return nil, 0, fmt.Errorf("DNS rcode %s", dns.RcodeToString[reply.Rcode])
	}

	var addrs []netip.Addr
	var ttl uint32
	for _, rr := range reply.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			continue // 跳过 CNAME，响应里通常已附带最终 A 记录
		}
		ip, ok := netip.AddrFromSlice(a.A.To4())
		if !ok {
			continue
		}
		addrs = append(addrs, ip)
		if ttl == 0 || a.Hdr.Ttl < ttl {
			ttl = a.Hdr.Ttl
		}
	}
	return addrs, ttl, nil
}

// JoinPort 把解析结果拼成可直接交给 netbind 的 "IP:port" 形式。
func JoinPort(ip netip.Addr, port int) string {
	return netip.AddrPortFrom(ip, uint16(port)).String()
}
