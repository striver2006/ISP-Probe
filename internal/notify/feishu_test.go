package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"isp-probe/internal/config"
)

// 用一个按官方算法独立复算签名的假飞书服务端，验证我们发出去的确实对得上。
//
// 单测里已经断言过 sign 的摆放位置，但那是拿自己的实现验自己。这里换个方向：
// 服务端不碰 buildRequest，只按飞书文档的 key = "<ts>\n<secret>"、data 为空
// 重算一遍 —— 加签方向搞反（钉钉恰好相反）在这里才会暴露。
func TestFeishuEndToEnd(t *testing.T) {
	const secret = "test-secret-xyz"
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)

		ts, _ := body["timestamp"].(string)
		// 官方算法：key = "<ts>\n<secret>"，data 为空
		mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
		want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if body["sign"] != want {
			w.Write([]byte(`{"code":19021,"msg":"sign match fail"}`))
			return
		}
		// 时间戳必须是秒且在合理窗口内（飞书要求 1 小时内）
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || time.Since(time.Unix(sec, 0)) > time.Hour {
			w.Write([]byte(`{"code":19021,"msg":"timestamp invalid"}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	}))
	defer srv.Close()

	wh := NewWebhook(config.WebhookConfig{
		Name: "飞书", Kind: config.KindFeishu, URL: srv.URL,
		Secret: secret, Timeout: 3 * time.Second,
	}, srv.Client(), testLogger())
	defer wh.Close(time.Second)

	if err := wh.Send(context.Background(), downEvent()); err != nil {
		t.Fatalf("飞书发送应成功，实得 %v（服务端按官方算法复算了签名）", err)
	}

	if body["msg_type"] != "text" {
		t.Errorf("msg_type 应为 text，实得 %v", body["msg_type"])
	}
	content, ok := body["content"].(map[string]any)
	if !ok || content["text"] == "" {
		t.Fatalf("content.text 缺失，实得 %v", body)
	}
	t.Logf("飞书实际收到的正文：\n%s", content["text"])
	t.Logf("完整请求体字段：%v", keysOf(body))
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
