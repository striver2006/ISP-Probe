//go:build darwin

package doctor

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"syscall"

	"golang.org/x/net/route"
)

// detectTUN 返回检测到的 TUN 接口名，以及路由表中是否存在分片默认路由。
//
// clash / mihomo 的 TUN 用 0.0.0.0/1 + 128.0.0.0/1 这对路由抢占出站流量：
// 它们比 0.0.0.0/0 更具体因而优先级更高，同时又不覆盖默认路由项本身
// （这也是本工具能靠默认路由找到真实物理网卡的原因）。
func detectTUN() (names []string, routeHijacked bool) {
	if ifis, err := net.Interfaces(); err == nil {
		for _, ifi := range ifis {
			if ifi.Flags&net.FlagUp == 0 || !strings.HasPrefix(ifi.Name, "utun") {
				continue
			}
			// 只关心配了可路由地址的 utun。macOS 自带若干只有 IPv6
			// link-local 地址的 utun（Apple 的后台服务），它们不参与
			// 流量劫持，列出来只会干扰判断。
			if hasRoutableAddr(&ifi) {
				names = append(names, ifi.Name)
			}
		}
	}

	rib, err := route.FetchRIB(syscall.AF_INET, route.RIBTypeRoute, 0)
	if err != nil {
		return names, false
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return names, false
	}
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Flags&syscall.RTF_UP == 0 {
			continue
		}
		if len(rm.Addrs) <= syscall.RTAX_NETMASK {
			continue
		}
		// /1 掩码即 128.0.0.0
		if mask, ok := rm.Addrs[syscall.RTAX_NETMASK].(*route.Inet4Addr); ok {
			if mask.IP == [4]byte{128, 0, 0, 0} {
				return names, true
			}
		}
	}
	return names, false
}

// hasRoutableAddr 判断接口是否配有非 link-local 的地址。
func hasRoutableAddr(ifi *net.Interface) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip, ok := netip.AddrFromSlice(n.IP); ok {
			ip = ip.Unmap()
			if !ip.IsLinkLocalUnicast() && !ip.IsLoopback() && !ip.IsUnspecified() {
				return true
			}
		}
	}
	return false
}

// explainBlockErr 解读连接错误是否为内核防火墙拦截。
//
// macOS 上 clash 只靠路由劫持，不会用防火墙封端口，绑定接口即可绕开。
func explainBlockErr(err error) (blocked bool, hint string) {
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return true, "连接被系统拒绝，可能存在防火墙规则"
	}
	return false, ""
}

// firewallHint 说明本平台放行入站连接需要做什么。
func firewallHint() string {
	return "首次有外部设备访问时，macOS 会弹窗询问是否允许接受传入连接，选「允许」"
}
