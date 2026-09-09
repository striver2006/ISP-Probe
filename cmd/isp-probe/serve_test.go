package main

import "testing"

// dialTarget 的意义是「单实例保护不会静默失效」。
//
// 0.0.0.0 不是可连接的目标：macOS 上碰巧落到环回，Windows 上直接
// WSAEADDRNOTAVAIL —— 于是 probeExisting 永远探测不到已在运行的实例，
// 能同时起两个进程抢同一个 SQLite 文件。
func TestDialTarget(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.0.0.0:8686", "127.0.0.1:8686"},
		{"[::]:8686", "127.0.0.1:8686"},
		{":8686", "127.0.0.1:8686"}, // 拼出的 "http://:8686/..." 根本不是合法 URL
		{"127.0.0.1:8686", "127.0.0.1:8686"},
		{"192.168.31.20:8686", "192.168.31.20:8686"}, // 绑了具体地址就照原样连
		{"garbage", "garbage"},                       // 解析不了就别自作主张
	}
	for _, c := range cases {
		if got := dialTarget(c.in); got != c.want {
			t.Errorf("dialTarget(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestDisplayURL(t *testing.T) {
	if got := displayURL("0.0.0.0:8686"); got != "http://127.0.0.1:8686" {
		t.Errorf("监听通配时应给出可点击的环回地址，实得 %q", got)
	}
}
