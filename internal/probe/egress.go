package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"isp-probe/internal/dnsx"
	"isp-probe/internal/netbind"
)

// Egress 是当前出口的身份：公网 IP 与其运营商归属。
//
// 这是测速流程的关键一环 —— 路由器按 MAC 绑定 WAN 口，主机无法直接得知
// 自己此刻走的是哪条线，只能反过来看「出去之后被世界看到的是谁」。
type Egress struct {
	IP   string
	ISP  string
	Org  string
	AS   string
	From string // 数据来源
}

// Describe 给出人类可读的描述。
func (e Egress) Describe() string {
	if e.ISP != "" {
		return fmt.Sprintf("%s (%s)", e.IP, e.ISP)
	}
	return e.IP
}

// LookupEgress 查询当前出口 IP 与运营商归属。
//
// 依次尝试各个数据源直到成功。所有请求都经绑定网卡发出、且目标域名由
// DoH 自行解析 —— 否则查到的会是 clash 出口而非真实家宽出口。
func LookupEgress(ctx context.Context, b *netbind.Binder, r *dnsx.Resolver, endpoints []string) (Egress, error) {
	client := newHTTPClient(b, r, 12*time.Second)
	var lastErr error

	for _, raw := range endpoints {
		e, err := lookupOne(ctx, client, raw)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", raw, err)
			continue
		}
		return e, nil
	}
	return Egress{}, fmt.Errorf("出口归属查询全部失败: %w", lastErr)
}

func lookupOne(ctx context.Context, client *http.Client, raw string) (Egress, error) {
	// URL 保留域名：dialer 会用 DoH 解析它，SNI 与 Host 头因而都正确。
	req, err := newRequest(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return Egress{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return Egress{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Egress{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return Egress{}, err
	}

	host := ""
	if u, err := url.Parse(raw); err == nil {
		host = u.Hostname()
	}

	// ip-api.com 风格的 JSON
	var j struct {
		Query string `json:"query"`
		ISP   string `json:"isp"`
		Org   string `json:"org"`
		AS    string `json:"as"`
		IP    string `json:"ip"`
	}
	if err := json.Unmarshal(body, &j); err == nil {
		addr := j.Query
		if addr == "" {
			addr = j.IP
		}
		if addr != "" {
			return Egress{IP: addr, ISP: j.ISP, Org: j.Org, AS: j.AS, From: host}, nil
		}
	}

	// 退化处理：纯文本只返回一个 IP
	txt := strings.TrimSpace(string(body))
	if len(txt) > 0 && len(txt) < 64 && !strings.ContainsAny(txt, " \n\t<") {
		return Egress{IP: txt, From: host}, nil
	}
	return Egress{}, fmt.Errorf("无法从响应中解析出口 IP")
}
