//go:build windows

package doctor

import (
	"errors"
	"net"
	"strings"

	"golang.org/x/sys/windows"
)

// detectTUN 返回检测到的 TUN 接口名。
//
// Windows 上 clash 使用 wintun（其 inf 把 IfType 写死为 IF_TYPE_PROP_VIRTUAL=53）。
// 这里只按名称做轻量识别；「是否已接管流量」不在这里判断 —— 通用逻辑会用
// 未绑定连接的本地端点作为行为证据，那比解析路由表更直接也更可靠。
func detectTUN() (names []string, routeHijacked bool) {
	ifis, err := net.Interfaces()
	if err != nil {
		return nil, false
	}
	for _, ifi := range ifis {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		n := strings.ToLower(ifi.Name)
		for _, kw := range []string{"wintun", "clash", "meta", "mihomo", "sing-box", "singbox", "tun"} {
			if strings.Contains(n, kw) {
				names = append(names, ifi.Name)
				break
			}
		}
	}
	return names, false
}

// explainBlockErr 解读连接错误是否为 WFP 拦截。
//
// mihomo 开启 strict-route 时会用 WFP 在 ALE_AUTH_CONNECT 层 BLOCK 掉所有
// 非 TUN 接口到远端 53 端口的连接，以及（TUN 无 IPv6 地址时）全部 IPv6 出站。
// 被拦截时 connect() 返回 WSAEACCES(10013)。
func explainBlockErr(err error) (blocked bool, hint string) {
	if errors.Is(err, windows.WSAEACCES) {
		return true, "被 clash 的 strict-route 防火墙规则拦截（WSAEACCES）；" +
			"在 Clash Verge 设置中关闭 strict-route 即可恢复"
	}
	return false, ""
}

// firewallHint 说明本平台放行入站连接需要做什么。
func firewallHint() string {
	return `Windows 防火墙默认拦截入站，需以管理员身份放行：` +
		`netsh advfirewall firewall add rule name="ISP-Probe" dir=in action=allow protocol=TCP localport=8686`
}
