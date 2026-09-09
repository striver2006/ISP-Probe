package web

import (
	"encoding/json"
	"net/http"
	"time"

	"isp-probe/internal/notify"
	"isp-probe/internal/store"
)

// notifyChannel 是一个通知渠道对外的展示形态。
//
// 注意这里**没有完整 URL**：面板可以开放到局域网，接口回显什么，同一个
// WiFi 下的任何设备就能拿到什么，而 webhook URL 里的 key 等于发消息的权限。
type notifyChannel struct {
	Name    string     `json:"name"`
	Kind    string     `json:"kind"`
	Enabled bool       `json:"enabled"`
	Target  string     `json:"target"` // 脱敏后的地址
	LastTry *time.Time `json:"last_try,omitempty"`
	LastOK  bool       `json:"last_ok"`
	LastErr string     `json:"last_err,omitempty"`
}

func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	hooks := s.cfg.Notify.Webhooks
	out := make([]notifyChannel, 0, len(hooks))

	// 已启用的渠道有活着的 Webhook 实例，能报出最近一次发送结果；
	// 禁用的只有配置，照样列出来，好让人看到「配了但没开」。
	live := make(map[string]*notify.Webhook)
	if s.notifier != nil {
		for _, c := range s.notifier.Channels() {
			if wh, ok := c.(*notify.Webhook); ok {
				live[wh.Name()] = wh
			}
		}
	}

	for _, h := range hooks {
		row := notifyChannel{
			Name:    h.Name,
			Kind:    h.Kind,
			Enabled: h.Enabled,
			Target:  notify.MaskURL(h.URL),
		}
		if wh, ok := live[h.Name]; ok {
			st := wh.Stat()
			if !st.LastTry.IsZero() {
				t := st.LastTry
				row.LastTry = &t
				row.LastOK = st.LastOK
				row.LastErr = st.LastErr
			}
		}
		out = append(out, row)
	}
	writeJSON(w, out)
}

// handleNotifyTest 向指定渠道（不指定则全部已启用的）发一条测试消息。
//
// 这是个写操作，但它不接受任何配置、也不回传密钥 —— 最坏情况是被诱导
// 往用户自己的群里发几条测试消息。
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)

	if s.notifier == nil {
		writeJSON(w, []testResult{})
		return
	}

	ev := store.Event{
		TS:      time.Now(),
		LinkID:  "_test",
		Kind:    "down",
		Message: "这是一条来自 ISP探针 的测试消息，收到说明通知链路正常。",
	}

	results := make([]testResult, 0, 4)
	for _, c := range s.notifier.Channels() {
		wh, ok := c.(*notify.Webhook)
		if !ok || (req.Name != "" && wh.Name() != req.Name) {
			continue
		}
		res := testResult{Name: wh.Name(), OK: true}
		if err := wh.Send(r.Context(), ev); err != nil {
			res.OK = false
			res.Err = err.Error()
		}
		results = append(results, res)
	}
	writeJSON(w, results)
}

type testResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
}
