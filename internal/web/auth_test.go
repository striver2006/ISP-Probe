package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func do(h http.Handler, path, remote, header, query string) int {
	url := path
	if query != "" {
		url += "?token=" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.RemoteAddr = remote
	if header != "" {
		req.Header.Set(tokenHeader, header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// token 留空 = 不鉴权。面板默认只监听 127.0.0.1，为本机自用强加令牌是净损失。
func TestAuthDisabledWhenNoToken(t *testing.T) {
	h := authMiddleware("", okHandler())
	if code := do(h, "/api/status", "192.168.31.50:1234", "", ""); code != http.StatusOK {
		t.Errorf("未配置 token 时应全部放行，实得 %d", code)
	}
}

func TestAuthEnforced(t *testing.T) {
	h := authMiddleware("s3cret", okHandler())

	// 环回必须放行：本机 CLI、service install 的存活自检、单实例检测
	// 都在打自己的 /api/status，挡住它们只会制造莫名其妙的故障。
	if code := do(h, "/api/status", "127.0.0.1:5555", "", ""); code != http.StatusOK {
		t.Errorf("环回来源应放行，实得 %d", code)
	}

	if code := do(h, "/api/status", "192.168.31.50:1234", "", ""); code != http.StatusUnauthorized {
		t.Errorf("外部无令牌应 401，实得 %d", code)
	}
	if code := do(h, "/api/status", "192.168.31.50:1234", "wrong", ""); code != http.StatusUnauthorized {
		t.Errorf("错误令牌应 401，实得 %d", code)
	}
	if code := do(h, "/api/status", "192.168.31.50:1234", "s3cret", ""); code != http.StatusOK {
		t.Errorf("header 传令牌应放行，实得 %d", code)
	}

	// SSE 的 EventSource 不支持自定义请求头，令牌只能走 query。
	// 这条挂了意味着面板在局域网上不再实时刷新。
	if code := do(h, "/api/stream", "192.168.31.50:1234", "", "s3cret"); code != http.StatusOK {
		t.Errorf("query 传令牌应放行（SSE 只能这么传），实得 %d", code)
	}

	// 静态页面永远放行，否则浏览器连输入令牌的界面都打不开。
	if code := do(h, "/", "192.168.31.50:1234", "", ""); code != http.StatusOK {
		t.Errorf("静态页面应放行，实得 %d", code)
	}
}
