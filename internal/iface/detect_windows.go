//go:build windows

package iface

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 接口类型常量。x/sys/windows 未定义 IF_TYPE_PROP_VIRTUAL，自行声明。
const (
	ifTypeEthernet    = 6   // IF_TYPE_ETHERNET_CSMACD
	ifTypeWireless    = 71  // IF_TYPE_IEEE80211
	ifTypePropVirtual = 53  // IF_TYPE_PROP_VIRTUAL —— wintun 的 inf 里写死的就是它
	ifTypeTunnel      = 131 // IF_TYPE_TUNNEL
	ifTypeLoopback    = 24  // IF_TYPE_SOFTWARE_LOOPBACK
)

// descBlacklist 用于挡掉 IfType 同为 Ethernet(6)、却并非真实物理网卡的适配器，
// 典型如 Hyper-V / WSL2 / Docker Desktop 创建的 vEthernet。
var descBlacklist = []string{
	"wintun", "tap-windows", "tap-win32", "openvpn", "wireguard", "tailscale",
	"zerotier", "warp", "cloudflare", "clash", "mihomo", "meta", "sing-box",
	"vmware", "virtualbox", "hyper-v", "vethernet", "npcap", "loopback",
	"bluetooth", "wan miniport", "docker", "wsl", "kernel debug", "teredo",
}

type adapter struct {
	name     string
	desc     string
	index4   uint32
	index6   uint32
	ifType   uint32
	metric   uint32
	mac      string
	ipv4     netip.Addr
	ipv6     netip.Addr
	gateway  netip.Addr
	hasGW    bool
}

// Detect 自动识别当前用于上网的物理网卡。
//
// Windows 上不能只看默认路由：clash 的 wintun 会把自己的路由 metric 压到
// 最低来抢占流量，按 metric 排序反而会选中它。所以先用 IfType 白名单加
// 描述黑名单把虚拟网卡整体排除，再在剩下的真实网卡里按 metric 择优。
func Detect() (PhysicalIface, error) {
	ads, err := adapters()
	if err != nil {
		return PhysicalIface{}, fmt.Errorf("枚举网络适配器失败: %w", err)
	}

	var cands []adapter
	for _, a := range ads {
		if !isPhysical(a) {
			continue
		}
		cands = append(cands, a)
	}
	if len(cands) == 0 {
		return PhysicalIface{}, fmt.Errorf("未找到可用的物理网卡（已排除 TUN/虚拟适配器）")
	}

	// 有网关的优先（没网关的不可能是上网出口），其次 metric 小的优先，
	// 最后有线优先于无线。
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].hasGW != cands[j].hasGW {
			return cands[i].hasGW
		}
		if cands[i].metric != cands[j].metric {
			return cands[i].metric < cands[j].metric
		}
		return cands[i].ifType == ifTypeEthernet && cands[j].ifType != ifTypeEthernet
	})

	return toPhysical(cands[0]), nil
}

func isPhysical(a adapter) bool {
	switch a.ifType {
	case ifTypePropVirtual, ifTypeTunnel, ifTypeLoopback:
		return false
	case ifTypeEthernet, ifTypeWireless:
		// 继续后续检查
	default:
		return false
	}
	if !a.ipv4.IsValid() || a.ipv4.IsLinkLocalUnicast() {
		return false
	}
	if a.mac == "" {
		return false // 无 MAC 的基本是虚拟适配器
	}
	blob := strings.ToLower(a.name + " " + a.desc)
	for _, kw := range descBlacklist {
		if strings.Contains(blob, kw) {
			return false
		}
	}
	return true
}

func toPhysical(a adapter) PhysicalIface {
	return PhysicalIface{
		Name:         a.name,
		Index4:       a.index4,
		Index6:       a.index6,
		IPv4:         a.ipv4,
		IPv6:         a.ipv6,
		Gateway:      a.gateway,
		MAC:          a.mac,
		IsWireless:   a.ifType == ifTypeWireless,
		HardwareDesc: a.desc,
	}
}

// adapters 调用 GetAdaptersAddresses 枚举所有适配器。
func adapters() ([]adapter, error) {
	const flags = windows.GAA_FLAG_INCLUDE_GATEWAYS |
		windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST

	var buf []byte
	size := uint32(16 << 10)
	for i := 0; i < 4; i++ {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(syscall.AF_UNSPEC, flags, 0,
			(*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			return nil, err
		}
		if i == 3 {
			return nil, err
		}
	}

	var out []adapter
	for p := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); p != nil; p = p.Next {
		if p.OperStatus != windows.IfOperStatusUp {
			continue
		}
		a := adapter{
			name:   windows.UTF16PtrToString(p.FriendlyName),
			desc:   windows.UTF16PtrToString(p.Description),
			index4: p.IfIndex,
			// Windows 上 IPv6 用的是另一个索引，不能与 IfIndex 混用。
			index6: p.Ipv6IfIndex,
			ifType: p.IfType,
			metric: p.Ipv4Metric,
			mac:    macString(p.PhysicalAddress[:], p.PhysicalAddressLength),
		}
		for ua := p.FirstUnicastAddress; ua != nil; ua = ua.Next {
			ip, ok := sockaddrToAddr(&ua.Address)
			if !ok {
				continue
			}
			if ip.Is4() && !a.ipv4.IsValid() {
				a.ipv4 = ip
			} else if ip.Is6() && ip.IsGlobalUnicast() && !a.ipv6.IsValid() {
				a.ipv6 = ip
			}
		}
		for ga := p.FirstGatewayAddress; ga != nil; ga = ga.Next {
			if ip, ok := sockaddrToAddr(&ga.Address); ok && ip.Is4() {
				a.gateway = ip
				a.hasGW = true
				break
			}
		}
		out = append(out, a)
	}
	return out, nil
}

func sockaddrToAddr(sa *windows.SocketAddress) (netip.Addr, bool) {
	ip := sa.IP()
	if ip == nil {
		return netip.Addr{}, false
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func macString(b []byte, n uint32) string {
	if n == 0 || int(n) > len(b) {
		return ""
	}
	return formatMAC(net.HardwareAddr(b[:n]))
}

// enrich 用适配器表补全硬件描述与无线标志（供 ByName 路径使用）。
func enrich(p *PhysicalIface) {
	ads, err := adapters()
	if err != nil {
		return
	}
	for _, a := range ads {
		if a.name != p.Name && a.index4 != p.Index4 {
			continue
		}
		p.HardwareDesc = a.desc
		p.IsWireless = a.ifType == ifTypeWireless
		p.Index6 = a.index6
		if !p.Gateway.IsValid() {
			p.Gateway = a.gateway
		}
		return
	}
}
