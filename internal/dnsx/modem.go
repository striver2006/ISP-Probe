package dnsx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/miekg/dns"

	"isp-probe/internal/netbind"
)

// ModemQuery 是一次经光猫的 DNS 探测结果。
type ModemQuery struct {
	Server  netip.Addr    // 光猫 LAN IP
	Name    string        // 查询的域名
	Forced  bool          // 是否为强制递归查询（随机子域名）
	RTT     time.Duration //
	Addrs   []netip.Addr  // 解析到的 A 记录
	Rcode   string        // DNS 响应码
	Timeout bool          // 是否超时（可能是线路断，也可能是光猫限速）
	Err     error
}

// OK 表示拿到了任何形式的 DNS 响应。
//
// 注意：对强制递归查询而言，NXDOMAIN 同样算成功 —— 它证明查询确实抵达了
// 权威服务器并带回了应答，也就证明这条线的上游是通的。只有拿不到任何响应
// 才是问题。
func (q ModemQuery) OK() bool { return q.Err == nil && !q.Timeout }

// ModemProbe 经指定光猫做 DNS 查询，以此判定该条 WAN 线路的连通性。
//
// 原理：向光猫 LAN IP 发 DNS 查询，包必然经路由器对应的 WAN 口出去，
// 由该运营商的递归 DNS 应答。这条通道完全不依赖路由器的分流策略配置。
//
// 实测约束：光猫 DNS 有速率限制，快速连发 5 次即全部超时，而慢节奏查询
// 一切正常。因此这里强制两次查询之间的最小间隔，否则会把自己造成的限速
// 误判为线路故障。
type ModemProbe struct {
	binder  *netbind.Binder
	minGap  time.Duration
	timeout time.Duration

	mu   sync.Mutex
	last map[netip.Addr]time.Time
}

func NewModemProbe(b *netbind.Binder, minGap, timeout time.Duration) *ModemProbe {
	if minGap <= 0 {
		minGap = 2 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ModemProbe{
		binder:  b,
		minGap:  minGap,
		timeout: timeout,
		last:    make(map[netip.Addr]time.Time),
	}
}

// throttle 阻塞到距上次查询同一光猫已满 minGap，避免触发其速率限制。
func (m *ModemProbe) throttle(ctx context.Context, server netip.Addr) error {
	m.mu.Lock()
	wait := time.Duration(0)
	if t, ok := m.last[server]; ok {
		if elapsed := time.Since(t); elapsed < m.minGap {
			wait = m.minGap - elapsed
		}
	}
	// 先占位，避免并发探测同一光猫时同时通过检查。
	m.last[server] = time.Now().Add(wait)
	m.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Query 经 server（光猫）解析 name。
func (m *ModemProbe) Query(ctx context.Context, server netip.Addr, name string) ModemQuery {
	return m.query(ctx, server, name, false)
}

// QueryForced 用随机子域名强制完整递归，验证该线路上游真的可达
// （而非仅仅命中了光猫本地缓存）。
func (m *ModemProbe) QueryForced(ctx context.Context, server netip.Addr, baseDomain string) ModemQuery {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		// 退化为时间戳，不影响探测语义。
		return m.query(ctx, server, fmt.Sprintf("p%d.%s", time.Now().UnixNano(), baseDomain), true)
	}
	return m.query(ctx, server, hex.EncodeToString(buf)+"."+baseDomain, true)
}

func (m *ModemProbe) query(ctx context.Context, server netip.Addr, name string, forced bool) ModemQuery {
	res := ModemQuery{Server: server, Name: name, Forced: forced}

	if err := m.throttle(ctx, server); err != nil {
		res.Err = err
		return res
	}

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(name), dns.TypeA)
	msg.RecursionDesired = true

	c := &dns.Client{
		Net:     "udp4",
		Dialer:  m.binder.Dialer("udp4", m.timeout),
		Timeout: m.timeout,
	}

	reply, rtt, err := c.ExchangeContext(ctx, msg, netip.AddrPortFrom(server, 53).String())
	res.RTT = rtt
	if err != nil {
		res.Err = err
		res.Timeout = isTimeout(err)
		return res
	}

	res.Rcode = dns.RcodeToString[reply.Rcode]
	for _, rr := range reply.Answer {
		if a, ok := rr.(*dns.A); ok {
			if ip, ok := netip.AddrFromSlice(a.A.To4()); ok {
				res.Addrs = append(res.Addrs, ip)
			}
		}
	}
	return res
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}

// Alive 检测光猫本身是否在线（TCP 连它的管理端口）。
//
// 这一步不经过 WAN，用于隔离故障域：DNS 查不通但光猫连得上，说明是
// 线路或限速问题；光猫都连不上，则是本地链路 / 光猫掉电。
func (m *ModemProbe) Alive(ctx context.Context, server netip.Addr, ports []int) (bool, time.Duration) {
	if len(ports) == 0 {
		ports = []int{80, 8080, 443}
	}
	for _, p := range ports {
		st := time.Now()
		conn, err := m.binder.DialContext(ctx, "tcp4", netip.AddrPortFrom(server, uint16(p)).String())
		if err == nil {
			conn.Close()
			return true, time.Since(st)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return false, 0
}
