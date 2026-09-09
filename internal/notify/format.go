package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"text/template"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/store"
)

// tplData 是 custom 模板可用的字段。
//
// 刻意不提供线路中文名 —— store.Event 里没有这个字段，而 Message 本身
// 已经是「电信 线路故障：…」的完整文案，够用了。
type tplData struct {
	Kind    string
	LinkID  string
	Message string
	Time    string
}

// buildRequest 按渠道类型拼出目标 URL、Content-Type 与请求体。
//
// 加签用标准库的 HMAC-SHA256，不引第三方 SDK —— 三家的算法都只是
// 「时间戳 + 密钥拼一拼再 base64」，为此拖进三个 SDK 不值得。
func buildRequest(c config.WebhookConfig, ev store.Event, now time.Time) (target, ctype string, body []byte, err error) {
	text := Title(ev.Kind) + "\n" + ev.Message + "\n" + now.Format("2006-01-02 15:04:05")
	target = c.URL
	ctype = "application/json; charset=utf-8"

	switch c.Kind {
	case config.KindWeCom:
		body, err = json.Marshal(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"content": markdown(ev, now)},
		})

	case config.KindDingTalk:
		if c.Secret != "" {
			// 钉钉：sign = base64(HMAC-SHA256(secret, "<ts>\n<secret>"))，随 query 传。
			ts := strconv.FormatInt(now.UnixMilli(), 10)
			sig := sign(c.Secret, ts+"\n"+c.Secret)
			target, err = appendQuery(c.URL, map[string]string{"timestamp": ts, "sign": sig})
			if err != nil {
				return "", "", nil, err
			}
		}
		body, err = json.Marshal(map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]string{
				"title": Title(ev.Kind),
				"text":  markdown(ev, now),
			},
		})

	case config.KindFeishu:
		payload := map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": text},
		}
		if c.Secret != "" {
			// 飞书：sign = base64(HMAC-SHA256(key="<ts>\n<secret>", data=""))，放 body。
			// 注意与钉钉的方向相反 —— 这里时间戳是秒且参与构成密钥。
			ts := strconv.FormatInt(now.Unix(), 10)
			payload["timestamp"] = ts
			payload["sign"] = sign(ts+"\n"+c.Secret, "")
		}
		body, err = json.Marshal(payload)

	case config.KindCustom:
		if c.ContentType != "" {
			ctype = c.ContentType
		}
		body, err = renderTemplate(c.BodyTemplate, ev, now)

	default:
		return "", "", nil, fmt.Errorf("未知的通知类型 %q", c.Kind)
	}
	return target, ctype, body, err
}

// markdown 是企微/钉钉共用的正文。两家的 markdown 子集都支持这几个标记。
func markdown(ev store.Event, now time.Time) string {
	return fmt.Sprintf("### %s\n%s\n> %s",
		Title(ev.Kind), ev.Message, now.Format("2006-01-02 15:04:05"))
}

func renderTemplate(s string, ev store.Event, now time.Time) ([]byte, error) {
	t, err := template.New("webhook").Parse(s)
	if err != nil {
		return nil, fmt.Errorf("渲染模板失败: %w", err)
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, tplData{
		Kind:    ev.Kind,
		LinkID:  ev.LinkID,
		Message: ev.Message,
		Time:    now.Format("2006-01-02 15:04:05"),
	})
	if err != nil {
		return nil, fmt.Errorf("渲染模板失败: %w", err)
	}
	return buf.Bytes(), nil
}

// sign 是三家加签共用的原语：base64(HMAC-SHA256(key, data))。
// 钉钉与飞书的差别只在 key/data 怎么摆，见各自的调用处。
func sign(key, data string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(data))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func appendQuery(raw string, kv map[string]string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("解析 webhook 地址失败: %w", err)
	}
	q := u.Query()
	for k, v := range kv {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// apiError 是三家共用的响应形状。
//
// 关键：企微/钉钉/飞书在「key 失效」「机器人被移出群」这些情况下**依然
// 返回 HTTP 200**，真正的错误码在 body 里。只看状态码会把失败当成功，
// 于是线路断了、消息没发出去，而日志里一片祥和。
type apiError struct {
	ErrCode int    `json:"errcode"` // 企微 / 钉钉
	ErrMsg  string `json:"errmsg"`
	Code    int    `json:"code"` // 飞书（新版）
	Msg     string `json:"msg"`
	// 飞书在签名校验失败这类场景下会走另一套 PascalCase 字段，且没有 code。
	// 少解析这一组的话，「签名配错了」会被当成发送成功。
	StatusCode int    `json:"StatusCode"`
	StatusMsg  string `json:"StatusMessage"`
}

// checkResponse 解析响应体，返回业务层错误（nil 表示成功）。
func checkResponse(kind string, raw []byte) error {
	// custom 渠道对端是什么形状我们不知道，只能以 HTTP 状态码为准。
	if kind == config.KindCustom {
		return nil
	}
	var r apiError
	if err := json.Unmarshal(raw, &r); err != nil {
		// 解析不出来不当作失败：状态码已经是 2xx，可能对方就是返回了个空体。
		return nil
	}
	if r.ErrCode != 0 {
		return fmt.Errorf("errcode=%d %s", r.ErrCode, r.ErrMsg)
	}
	if r.StatusCode != 0 {
		return fmt.Errorf("code=%d %s", r.StatusCode, r.StatusMsg)
	}
	if r.Code != 0 {
		return fmt.Errorf("code=%d %s", r.Code, r.Msg)
	}
	return nil
}
