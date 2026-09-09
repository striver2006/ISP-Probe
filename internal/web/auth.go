package web

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

// tokenHeader 是携带访问令牌的请求头。
const tokenHeader = "X-ISP-Token"

// authMiddleware 在配置了 web.token 时，要求非本机请求带上正确的令牌。
//
// 三条刻意的设计：
//
//   - token 为空 = 不鉴权。面板默认只监听 127.0.0.1，为本机自用强加一道
//     令牌是净损失；只有把 listen 改成 0.0.0.0 开放到局域网时才需要。
//   - 环回来源直接放行。本机 CLI、service install 的存活自检、单实例检测
//     都在打自己的 /api/status，把它们挡在门外只会制造莫名其妙的故障。
//   - 静态页面不鉴权。否则浏览器连输入令牌的界面都打不开。
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || isLoopback(r.RemoteAddr) || tokenOK(token, r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"需要访问令牌"}`))
	})
}

func tokenOK(token string, r *http.Request) bool {
	got := r.Header.Get(tokenHeader)
	if got == "" {
		// SSE 只能走 query：EventSource 不支持自定义请求头，这不是偷懒。
		// 想把这里改成「只认 header」的话，面板会停止刷新。
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
