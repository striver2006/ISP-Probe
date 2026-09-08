// Package iface 负责识别本机用于上网的物理网卡，并排除 clash TUN 等虚拟接口。
//
// 关键洞察：clash / mihomo 的 TUN 是用 0.0.0.0/1 + 128.0.0.0/1 这对分片路由
// 抢占流量的，它**不覆盖** 0.0.0.0/0。因此系统的默认路由项依然指向真实的
// 物理网卡，是最可靠的判据；名字与类型黑名单只作为交叉校验。
package iface

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// PhysicalIface 描述一块可用于探测的物理网卡。
type PhysicalIface struct {
	Name    string     // 接口名，如 "en0"
	Index4  uint32     // IPv4 接口索引
	Index6  uint32     // IPv6 接口索引（Windows 上可能与 Index4 不同）
	IPv4    netip.Addr // 网卡 IPv4
	IPv6    netip.Addr // 网卡全局 IPv6（可能无效）
	Gateway netip.Addr // 默认网关，通常即家用路由器地址
	MAC     string     // 大写冒号分隔，如 5C:9B:A6:76:78:B1

	// IsWireless 影响测速结果的解读：Wi-Fi 链路的吞吐上限受无线速率制约，
	// 跑不满千兆宽带，UI 需要据此提示用户。
	IsWireless bool
	// HardwareDesc 是平台给出的硬件描述，便于排障展示。
	HardwareDesc string
}

func (p PhysicalIface) String() string {
	kind := "有线"
	if p.IsWireless {
		kind = "Wi-Fi"
	}
	return fmt.Sprintf("%s (%s, %s, MAC %s, 网关 %s)", p.Name, kind, p.IPv4, p.MAC, p.Gateway)
}

// virtualPrefixes 是各平台通用的虚拟接口名前缀黑名单。
var virtualPrefixes = []string{
	"utun", "tun", "tap", "ipsec", "ppp", "awdl", "llw",
	"bridge", "gif", "stf", "lo", "anpi", "vmnet", "vboxnet",
}

// IsVirtualName 按接口名判断是否为虚拟接口。
func IsVirtualName(name string) bool {
	n := strings.ToLower(name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	// macOS 的 apN 是 AirPlay/热点接口，但要避免误伤真实的 "ap" 开头网卡名，
	// 这里只匹配 ap + 数字。
	if len(n) >= 3 && strings.HasPrefix(n, "ap") && n[2] >= '0' && n[2] <= '9' {
		return true
	}
	return false
}

// formatMAC 把硬件地址格式化为大写冒号分隔形式，方便用户抄到路由器配置页。
func formatMAC(hw net.HardwareAddr) string {
	if len(hw) == 0 {
		return ""
	}
	return strings.ToUpper(hw.String())
}

// firstGlobalIPv4 返回接口上第一个非 link-local 的 IPv4。
func firstGlobalIPv4(ifi *net.Interface) netip.Addr {
	addrs, err := ifi.Addrs()
	if err != nil {
		return netip.Addr{}
	}
	for _, a := range addrs {
		pfx, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(pfx.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ip.Is4() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() && !ip.IsUnspecified() {
			return ip
		}
	}
	return netip.Addr{}
}

// firstGlobalIPv6 返回接口上第一个全局单播 IPv6。
func firstGlobalIPv6(ifi *net.Interface) netip.Addr {
	addrs, err := ifi.Addrs()
	if err != nil {
		return netip.Addr{}
	}
	for _, a := range addrs {
		pfx, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(pfx.IP)
		if !ok {
			continue
		}
		if ip.Is6() && !ip.Is4In6() && ip.IsGlobalUnicast() {
			return ip
		}
	}
	return netip.Addr{}
}

// ByName 按接口名精确查找，用于用户在配置里强制指定网卡的场景。
func ByName(name string) (PhysicalIface, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return PhysicalIface{}, fmt.Errorf("找不到网卡 %q: %w", name, err)
	}
	if ifi.Flags&net.FlagUp == 0 {
		return PhysicalIface{}, fmt.Errorf("网卡 %q 未启用", name)
	}
	p := PhysicalIface{
		Name:   ifi.Name,
		Index4: uint32(ifi.Index),
		Index6: uint32(ifi.Index),
		IPv4:   firstGlobalIPv4(ifi),
		IPv6:   firstGlobalIPv6(ifi),
		MAC:    formatMAC(ifi.HardwareAddr),
	}
	if !p.IPv4.IsValid() {
		return PhysicalIface{}, fmt.Errorf("网卡 %q 没有可用的 IPv4 地址", name)
	}
	enrich(&p)
	return p, nil
}
