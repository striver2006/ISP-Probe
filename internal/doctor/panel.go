package doctor

import (
	"fmt"
	"net"
	"net/netip"

	"isp-probe/internal/config"
	"isp-probe/internal/iface"
)

// checkPanel 说明面板的可达范围，并在开放到局域网却没设令牌时告警。
//
// 这一项不测网络，纯粹是把「你现在的配置意味着什么」讲清楚：面板监听
// 0.0.0.0 之后，同一个 WiFi 下的任何设备（包括客人的手机和 IoT 设备）都能
// 看到内网拓扑、光猫地址、网卡 MAC，还能触发一次跑满带宽的测速。
func (r *Report) checkPanel(cfg config.Config, p iface.PhysicalIface) {
	host, port, err := net.SplitHostPort(cfg.Web.Listen)
	if err != nil {
		r.add(Check{
			Name:   "面板监听地址",
			Level:  LevelFail,
			Detail: fmt.Sprintf("web.listen %q 不是合法的 host:port", cfg.Web.Listen),
			Hint:   "改成 127.0.0.1:8686（仅本机）或 0.0.0.0:8686（局域网可访问）",
		})
		return
	}

	c := Check{Name: "面板监听地址"}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		lan := "（网卡地址未知）"
		if p.IPv4.IsValid() {
			lan = "http://" + net.JoinHostPort(p.IPv4.String(), port)
		}
		if cfg.Web.Token == "" {
			c.Level = LevelWarn
			c.Detail = fmt.Sprintf("面板对局域网开放且未设访问令牌，%s", lan)
			c.Hint = "任何连上同一网络的设备都能查看拓扑信息并触发测速。" +
				"建议在 config.local.yaml 里设 web.token"
		} else {
			c.Level = LevelOK
			c.Detail = fmt.Sprintf("面板对局域网开放（已启用访问令牌），%s", lan)
			c.Hint = firewallHint()
		}
	default:
		c.Level = LevelOK
		c.Detail = fmt.Sprintf("面板仅本机可访问，http://%s", cfg.Web.Listen)
		if isLoopbackHost(host) {
			c.Hint = "如需从手机等设备访问，把 web.listen 改成 0.0.0.0:" + port + " 并设置 web.token"
		}
	}
	r.add(c)
}

func isLoopbackHost(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
