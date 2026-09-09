package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func downEvent() store.Event {
	return store.Event{TS: time.Now(), LinkID: "telecom", Kind: "down", Message: "电信 线路故障：光猫不可达"}
}

// 三家 IM 的请求体形状各不相同，发错了对端只会静默丢弃。
func TestBuildRequestShape(t *testing.T) {
	now := time.Unix(1757400000, 0)
	cases := []struct {
		kind     string
		wantKeys []string // 请求体 JSON 顶层应有的字段
	}{
		{config.KindWeCom, []string{"msgtype", "markdown"}},
		{config.KindDingTalk, []string{"msgtype", "markdown"}},
		{config.KindFeishu, []string{"msg_type", "content"}},
	}
	for _, c := range cases {
		_, ctype, body, err := buildRequest(
			config.WebhookConfig{Kind: c.kind, URL: "https://example.com/hook"}, downEvent(), now)
		if err != nil {
			t.Fatalf("%s: buildRequest 失败: %v", c.kind, err)
		}
		if !strings.HasPrefix(ctype, "application/json") {
			t.Errorf("%s: Content-Type 应为 JSON，实得 %q", c.kind, ctype)
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("%s: 请求体不是合法 JSON: %v", c.kind, err)
		}
		for _, k := range c.wantKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("%s: 请求体缺少字段 %q，实得 %v", c.kind, k, m)
			}
		}
	}
}

// 加签算法两家方向相反：钉钉拿 secret 当 key，飞书拿「时间戳\nsecret」当 key。
// 写反了对端一律返回签名校验失败，而错误信息不会告诉你是搞反了。
func TestSignPlacement(t *testing.T) {
	now := time.Unix(1757400000, 0)

	target, _, _, err := buildRequest(config.WebhookConfig{
		Kind: config.KindDingTalk, URL: "https://oapi.dingtalk.com/robot/send?access_token=abc", Secret: "s3cret",
	}, downEvent(), now)
	if err != nil {
		t.Fatalf("钉钉 buildRequest 失败: %v", err)
	}
	if !strings.Contains(target, "sign=") || !strings.Contains(target, "timestamp=") {
		t.Errorf("钉钉的签名应拼进 query，实得 %s", target)
	}
	wantSig := sign("s3cret", "1757400000000\ns3cret")
	if !strings.Contains(target, "sign="+urlEscape(wantSig)) {
		t.Errorf("钉钉签名不符，期望包含 %s，实得 %s", wantSig, target)
	}

	_, _, body, err := buildRequest(config.WebhookConfig{
		Kind: config.KindFeishu, URL: "https://open.feishu.cn/hook/xyz", Secret: "s3cret",
	}, downEvent(), now)
	if err != nil {
		t.Fatalf("飞书 buildRequest 失败: %v", err)
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	if m["sign"] != sign("1757400000\ns3cret", "") {
		t.Errorf("飞书签名应放在 body 且用「时间戳\\nsecret」作 key，实得 %v", m["sign"])
	}
}

func urlEscape(s string) string {
	r := strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D")
	return r.Replace(s)
}

// custom 渠道走模板渲染，字段名写错应该在配置校验期就暴露，这里验渲染结果。
func TestCustomTemplate(t *testing.T) {
	_, ctype, body, err := buildRequest(config.WebhookConfig{
		Kind: config.KindCustom, URL: "https://example.com/hook",
		ContentType:  "text/plain",
		BodyTemplate: "{{.Kind}}|{{.LinkID}}|{{.Message}}",
	}, downEvent(), time.Now())
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if ctype != "text/plain" {
		t.Errorf("custom 应使用配置的 content_type，实得 %q", ctype)
	}
	want := "down|telecom|电信 线路故障：光猫不可达"
	if string(body) != want {
		t.Errorf("渲染结果不符\n期望 %s\n实得 %s", want, string(body))
	}
}

// 这是最容易踩的坑：企微/钉钉/飞书在 key 失效时**依然返回 HTTP 200**，
// 错误码藏在 body 里。只看状态码会把彻底发不出去的渠道当成正常。
func TestErrCodeInBodyIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"errcode":93000,"errmsg":"invalid webhook url"}`))
	}))
	defer srv.Close()

	wh := NewWebhook(config.WebhookConfig{
		Name: "t", Kind: config.KindWeCom, URL: srv.URL, Timeout: 2 * time.Second,
	}, srv.Client(), testLogger())
	defer wh.Close(time.Second)

	err := wh.Send(context.Background(), downEvent())
	if err == nil {
		t.Fatal("HTTP 200 但 errcode≠0 必须判为失败，实得 nil")
	}
	if !strings.Contains(err.Error(), "93000") {
		t.Errorf("错误信息应带上 errcode，实得 %v", err)
	}
	if st := wh.Stat(); st.LastOK {
		t.Error("失败后 Stat().LastOK 应为 false")
	}
}

func TestSendSuccessRecordsStat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") == "" {
			t.Error("请求应带 Content-Type")
		}
		if r.ContentLength <= 0 {
			t.Error("请求应显式设置 Content-Length")
		}
		w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	wh := NewWebhook(config.WebhookConfig{
		Name: "t", Kind: config.KindWeCom, URL: srv.URL, Timeout: 2 * time.Second,
	}, srv.Client(), testLogger())
	defer wh.Close(time.Second)

	if err := wh.Send(context.Background(), downEvent()); err != nil {
		t.Fatalf("发送应成功，实得 %v", err)
	}
	st := wh.Stat()
	if !st.LastOK || st.LastTry.IsZero() {
		t.Errorf("成功后应记录 Stat，实得 %+v", st)
	}
}

// Events 为空表示订阅全部；配了就只收订阅的那几类。
func TestWantsFilter(t *testing.T) {
	all := &Webhook{cfg: config.WebhookConfig{}}
	if !all.Wants("down") || !all.Wants("up") || !all.Wants("down_repeat") {
		t.Error("events 留空应订阅全部事件")
	}
	only := &Webhook{cfg: config.WebhookConfig{Events: []string{"down"}}}
	if !only.Wants("down") {
		t.Error("应收 down")
	}
	if only.Wants("up") || only.Wants("down_repeat") {
		t.Error("未订阅的事件不应下发")
	}
}

// 队列满了必须丢弃而不是阻塞：Notify 是在探测循环里同步调用的，
// 一旦卡住，所有线路的采样都会停摆。
func TestQueueFullDropsInsteadOfBlocking(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	wh := NewWebhook(config.WebhookConfig{
		Name: "t", Kind: config.KindWeCom, URL: srv.URL, Timeout: time.Minute,
	}, srv.Client(), testLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queueSize*3; i++ {
			wh.Notify(context.Background(), downEvent())
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("队列满时 Notify 阻塞了，会拖垮整个探测循环")
	}
}

// 退出时队列里往往正压着那条最该发出去的 down 通知。
func TestCloseFlushesQueue(t *testing.T) {
	got := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- struct{}{}
		w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()

	wh := NewWebhook(config.WebhookConfig{
		Name: "t", Kind: config.KindWeCom, URL: srv.URL, Timeout: 2 * time.Second,
	}, srv.Client(), testLogger())

	wh.Notify(context.Background(), downEvent())
	wh.Close(3 * time.Second)

	select {
	case <-got:
	default:
		t.Fatal("Close 应把队列里积压的通知发完")
	}
}

// webhook URL 里的 key 就是密钥：日志会落盘，/api/notify 对整个局域网可见。
func TestMaskURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcdef123456", "qyapi.weixin.qq.com/…3456"},
		{"https://oapi.dingtalk.com/robot/send?access_token=zzzz9999", "oapi.dingtalk.com/…9999"},
		{"https://open.feishu.cn/open-apis/bot/v2/hook/uuid-tail", "open.feishu.cn/…tail"},
		{"not a url", "(地址无效)"},
	}
	for _, c := range cases {
		if got := MaskURL(c.in); got != c.want {
			t.Errorf("MaskURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// 最重要的一条：脱敏结果里绝不能出现完整密钥。
	if strings.Contains(MaskURL("https://x.com/a?key=SUPERSECRETVALUE"), "SUPERSECRETVALUE") {
		t.Error("脱敏后仍包含完整密钥")
	}
}

// 飞书的失败响应有两套形状：新版 {"code":..,"msg":..}，
// 而签名校验失败这类会走 PascalCase 的 {"StatusCode":..,"StatusMessage":..}，
// 且不带 code 字段。漏掉后一组的话，「secret 配错了」会显示成发送成功。
func TestFeishuErrorShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"新版 code", `{"code":19021,"msg":"sign match fail"}`, "19021"},
		{"PascalCase", `{"Extra":null,"StatusCode":9499,"StatusMessage":"sign match fail"}`, "9499"},
		{"成功", `{"code":0,"msg":"success","data":{}}`, ""},
		{"成功-旧版", `{"Extra":null,"StatusCode":0,"StatusMessage":"success"}`, ""},
	}
	for _, c := range cases {
		err := checkResponse(config.KindFeishu, []byte(c.body))
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: 应判为成功，实得 %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 应判为失败，实得 nil", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 错误应含 %s，实得 %v", c.name, c.want, err)
		}
	}
}
