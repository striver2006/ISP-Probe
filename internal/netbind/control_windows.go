//go:build windows

package netbind

import (
	"encoding/binary"
	"errors"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 的接口绑定 socket 选项。
//
// golang.org/x/sys/windows 没有定义这两个常量（它只有 IP_MULTICAST_IF 等），
// 必须自己声明。两者的值都是 31，靠 level 区分协议族。
//
// 这与 WireGuard-for-Windows 和 Tailscale 绕过自身 TUN 的手段完全相同：
// wintun 同样是用 0.0.0.0/1 + 128.0.0.0/1 分片默认路由接管全局流量的，
// 它们的加密隧道出站必须钉在物理网卡上，否则立刻路由环路。
const (
	ipUnicastIF   = 31 // IP_UNICAST_IF,   level IPPROTO_IP，值需**网络字节序**
	ipv6UnicastIF = 31 // IPV6_UNICAST_IF, level IPPROTO_IPV6，值为主机字节序
)

// errNoProtoOpt 表示 socket 不支持该协议族的选项，忽略即可。
func errNoProtoOpt(err error) bool {
	return errors.Is(err, windows.WSAENOPROTOOPT) || errors.Is(err, windows.WSAEINVAL)
}

func control(idx4, idx6 uint32) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		cerr := c.Control(func(fd uintptr) {
			h := windows.Handle(fd)

			set4 := func() error {
				if idx4 == 0 {
					return nil
				}
				// MSDN: IPv4 的 IP_UNICAST_IF 要求接口索引以网络字节序传入
				// （"like an IP address with leading zeros"）。先转大端字节，
				// 再按原生 uint32 重新解释，等价于 htonl()。
				var b [4]byte
				binary.BigEndian.PutUint32(b[:], idx4)
				v := *(*uint32)(unsafe.Pointer(&b[0]))
				return windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIF, int(v))
			}
			set6 := func() error {
				if idx6 == 0 {
					return nil
				}
				// IPv6 是主机字节序，不转换。
				return windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIF, int(idx6))
			}

			switch {
			case strings.HasSuffix(network, "4"):
				serr = set4()
			case strings.HasSuffix(network, "6"):
				serr = set6()
			default:
				if err := set6(); err != nil && !errNoProtoOpt(err) {
					serr = err
					return
				}
				if err := set4(); err != nil && !errNoProtoOpt(err) {
					serr = err
				}
			}
		})
		if cerr != nil {
			return cerr
		}
		return serr
	}
}
