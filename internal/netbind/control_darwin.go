//go:build darwin

package netbind

import (
	"errors"
	"strings"
	"syscall"
)

// macOS 的接口绑定 socket 选项。
//
// 与 Windows 不同，这两个值都接受**主机字节序**的接口索引，无需转换。
//
// 设置后，出站包直接投递到指定接口，不再经过路由表选路 —— 这正是绕过
// clash TUN 的关键：TUN 是靠 0/1 + 128/1 这类分片默认路由抢占流量的，
// 绕开路由表查找它就失效了。
const (
	ipBoundIF   = 25  // IP_BOUND_IF,   level IPPROTO_IP
	ipv6BoundIF = 125 // IPV6_BOUND_IF, level IPPROTO_IPV6
)

// errNoProtoOpt 表示该 socket 不支持这个协议族的选项（例如在 AF_INET
// socket 上设置 IPv6 选项）。这种情况不是错误，忽略即可。
func errNoProtoOpt(err error) bool {
	return errors.Is(err, syscall.ENOPROTOOPT) || errors.Is(err, syscall.EINVAL)
}

func control(idx4, idx6 uint32) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		cerr := c.Control(func(fd uintptr) {
			set4 := func() error {
				if idx4 == 0 {
					return nil
				}
				return syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipBoundIF, int(idx4))
			}
			set6 := func() error {
				if idx6 == 0 {
					return nil
				}
				return syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6BoundIF, int(idx6))
			}

			switch {
			case strings.HasSuffix(network, "4"):
				serr = set4()
			case strings.HasSuffix(network, "6"):
				serr = set6()
			default:
				// "tcp" / "udp" 可能落在 dual-stack 的 AF_INET6 socket 上，
				// 两个都设，各自忽略「不支持」的错误。
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
