//go:build darwin

package iface

import (
	"bufio"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/net/route"
)

// Detect 自动识别当前用于上网的物理网卡。
//
// 策略：以系统默认路由（0.0.0.0/0）指向的接口为准 —— clash TUN 用的是
// 0/1 + 128/1 分片路由，不会覆盖默认路由项，所以它依然指向真实物理网卡。
// 随后用名字黑名单交叉校验，若默认路由确实落在虚拟接口上（说明遇到了
// 会改写默认路由的其它 VPN），再回退到枚举候选网卡。
func Detect() (PhysicalIface, error) {
	idx, gw, err := defaultRoute()
	if err == nil && idx > 0 {
		if ifi, err2 := net.InterfaceByIndex(idx); err2 == nil && !IsVirtualName(ifi.Name) {
			if ip := firstGlobalIPv4(ifi); ip.IsValid() {
				p := PhysicalIface{
					Name:    ifi.Name,
					Index4:  uint32(ifi.Index),
					Index6:  uint32(ifi.Index),
					IPv4:    ip,
					IPv6:    firstGlobalIPv6(ifi),
					Gateway: gw,
					MAC:     formatMAC(ifi.HardwareAddr),
				}
				enrich(&p)
				return p, nil
			}
		}
	}

	// 回退：枚举所有 up 且有全局 IPv4 的非虚拟网卡。
	ifis, err := net.Interfaces()
	if err != nil {
		return PhysicalIface{}, fmt.Errorf("枚举网卡失败: %w", err)
	}
	for _, ifi := range ifis {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagRunning == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback != 0 || IsVirtualName(ifi.Name) {
			continue
		}
		ip := firstGlobalIPv4(&ifi)
		if !ip.IsValid() {
			continue
		}
		p := PhysicalIface{
			Name:    ifi.Name,
			Index4:  uint32(ifi.Index),
			Index6:  uint32(ifi.Index),
			IPv4:    ip,
			IPv6:    firstGlobalIPv6(&ifi),
			Gateway: gw,
			MAC:     formatMAC(ifi.HardwareAddr),
		}
		enrich(&p)
		return p, nil
	}
	return PhysicalIface{}, fmt.Errorf("未找到可用的物理网卡（已排除 TUN/虚拟接口）")
}

// defaultRoute 解析 BSD 路由表，返回默认路由的接口索引与网关。
// 使用 golang.org/x/net/route 直接读 sysctl，避免 exec route 命令。
func defaultRoute() (int, netip.Addr, error) {
	rib, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return 0, netip.Addr{}, fmt.Errorf("读取路由表失败: %w", err)
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return 0, netip.Addr{}, fmt.Errorf("解析路由表失败: %w", err)
	}
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok {
			continue
		}
		// 必须是启用中的、经网关转发的路由。
		if rm.Flags&syscall.RTF_UP == 0 || rm.Flags&syscall.RTF_GATEWAY == 0 {
			continue
		}
		if len(rm.Addrs) <= syscall.RTAX_GATEWAY {
			continue
		}
		dst, ok := rm.Addrs[syscall.RTAX_DST].(*route.Inet4Addr)
		if !ok || dst.IP != [4]byte{0, 0, 0, 0} {
			continue
		}
		// 排除 clash TUN 的 0/1、128/1：它们的目的地址虽然也可能以 0 开头，
		// 但掩码不是 /0。这里显式校验掩码为全零。
		if len(rm.Addrs) > syscall.RTAX_NETMASK {
			if mask, ok := rm.Addrs[syscall.RTAX_NETMASK].(*route.Inet4Addr); ok {
				if mask.IP != [4]byte{0, 0, 0, 0} {
					continue
				}
			}
		}
		gw := netip.Addr{}
		if g, ok := rm.Addrs[syscall.RTAX_GATEWAY].(*route.Inet4Addr); ok {
			gw = netip.AddrFrom4(g.IP)
		}
		return rm.Index, gw, nil
	}
	return 0, netip.Addr{}, fmt.Errorf("未找到默认路由")
}

var (
	hwPortsOnce sync.Once
	hwPorts     map[string]string // device -> hardware port name
)

// loadHardwarePorts 读取 macOS 的硬件端口表，用于判断网卡是有线还是 Wi-Fi。
// 只在进程内执行一次并缓存；失败不致命，仅让 IsWireless 保持 false。
func loadHardwarePorts() {
	hwPorts = make(map[string]string)
	out, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return
	}
	var port string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Hardware Port:"):
			port = strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
		case strings.HasPrefix(line, "Device:"):
			dev := strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
			if dev != "" && port != "" {
				hwPorts[dev] = port
			}
		}
	}
}

// enrich 补充平台相关的展示信息。
func enrich(p *PhysicalIface) {
	hwPortsOnce.Do(loadHardwarePorts)
	if port, ok := hwPorts[p.Name]; ok {
		p.HardwareDesc = port
		lp := strings.ToLower(port)
		p.IsWireless = strings.Contains(lp, "wi-fi") || strings.Contains(lp, "airport") || strings.Contains(lp, "wlan")
	}
}
