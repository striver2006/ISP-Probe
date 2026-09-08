package probe

import (
	"context"
	"time"

	"isp-probe/internal/netbind"
)

// AnchorRTT 用 TCP connect 测量到一组锚点的往返时延，返回最小值。
//
// 为什么用 TCP connect 而非 ICMP 作为延迟主指标：
//   - 无需任何特权，Windows 上也不必绕道 iphlpapi
//   - 绑定接口对它的效果已实测确认可靠
//   - 走的是正常数据面，而路由器常对 ICMP 限速或降权，ICMP 读数会偏高
//
// 代价是测不准丢包率（SYN 重传由内核控制，一次超时无法区分丢了几个包），
// 丢包因此另行用 ICMP 测量。
//
// 取多个锚点的最小值是为了削弱单个目标服务器抖动带来的噪声。
func AnchorRTT(ctx context.Context, b *netbind.Binder, anchors []string, timeout time.Duration) (time.Duration, error) {
	var best time.Duration
	var lastErr error

	for _, a := range anchors {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		st := time.Now()
		conn, err := b.DialContext(cctx, "tcp4", a)
		d := time.Since(st)
		cancel()

		if err != nil {
			lastErr = err
			continue
		}
		conn.Close()
		if best == 0 || d < best {
			best = d
		}
	}
	if best == 0 {
		return 0, lastErr
	}
	return best, nil
}
