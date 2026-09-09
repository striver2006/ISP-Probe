package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/httpx"
	"isp-probe/internal/store"
)

// queueSize 是单个渠道的待发队列长度。
//
// 告警本就稀疏（一次故障一两条），16 条足够吸收「两条线同时抖动 + 重复
// 提醒」的峰值。满了宁可丢弃也不阻塞 —— 见 Notify 的注释。
const queueSize = 16

// retryBackoff 是失败后的重试间隔。总共尝试 3 次。
var retryBackoff = []time.Duration{time.Second, 3 * time.Second}

// Stat 是一个渠道最近一次发送的结果，供面板展示。
type Stat struct {
	LastTry time.Time
	LastOK  bool
	LastErr string
}

// Webhook 把事件发到 IM 群机器人。
//
// 发送是异步的：状态机在 RunOnce 的落库循环里同步调用 Notify，一次 8 秒
// 超时的 HTTP 请求会直接把整轮探测拖住。这里只入队，由 worker 慢慢发。
type Webhook struct {
	cfg    config.WebhookConfig
	client *http.Client
	log    *slog.Logger

	queue chan store.Event
	done  chan struct{}
	once  sync.Once

	mu   sync.Mutex
	stat Stat
}

// NewWebhook 构造一个渠道并启动其 worker。
//
// client 由调用方注入而不是内部构造：生产环境要的是绑定物理网卡 + DoH 的
// 客户端（httpx.NewClient），而测试要打 httptest 起的环回地址，绑了网卡就连不上。
func NewWebhook(cfg config.WebhookConfig, client *http.Client, log *slog.Logger) *Webhook {
	w := &Webhook{
		cfg:    cfg,
		client: client,
		log:    log,
		queue:  make(chan store.Event, queueSize),
		done:   make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *Webhook) Name() string { return w.cfg.Name }

// Kind 返回渠道类型，供面板展示。
func (w *Webhook) Kind() string { return w.cfg.Kind }

// Target 返回脱敏后的目标地址。
func (w *Webhook) Target() string { return MaskURL(w.cfg.URL) }

// Stat 返回最近一次发送结果。
func (w *Webhook) Stat() Stat {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stat
}

// Wants 判断该渠道是否订阅了这类事件。
func (w *Webhook) Wants(kind string) bool {
	if len(w.cfg.Events) == 0 {
		return true
	}
	for _, e := range w.cfg.Events {
		if e == kind {
			return true
		}
	}
	return false
}

// Notify 把事件放进待发队列，立刻返回。
//
// 返回 nil 只代表「已受理」。队列满时丢弃而不是阻塞：宁可漏一条告警，
// 也不能让通知把探测循环卡住 —— 那会让所有线路的采样一起停摆。
func (w *Webhook) Notify(_ context.Context, ev store.Event) error {
	if !w.Wants(ev.Kind) {
		return nil
	}
	select {
	case w.queue <- ev:
		return nil
	default:
		return fmt.Errorf("待发队列已满，丢弃一条 %s 通知", ev.Kind)
	}
}

// Send 同步发送一条事件，供 notify test 与面板的测试按钮使用。
func (w *Webhook) Send(ctx context.Context, ev store.Event) error {
	return w.deliver(ctx, ev)
}

func (w *Webhook) run() {
	defer close(w.done)
	for ev := range w.queue {
		// 队列里的事件不跟随探测的 ctx：进程退出时那个 ctx 已经取消，
		// 沿用它会让最后一条告警必然发不出去 —— 而那往往正是最该发的一条。
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), w.cfg.Timeout)
		err := w.deliverWithRetry(ctx, ev)
		cancel()
		if err != nil {
			w.log.Warn("发送 IM 通知失败", "渠道", w.cfg.Name, "目标", w.Target(), "err", err)
		}
	}
}

func (w *Webhook) deliverWithRetry(ctx context.Context, ev store.Event) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = w.deliver(ctx, ev); err == nil {
			return nil
		}
		if attempt >= len(retryBackoff) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(retryBackoff[attempt]):
		}
	}
}

func (w *Webhook) deliver(ctx context.Context, ev store.Event) error {
	err := w.post(ctx, ev)

	w.mu.Lock()
	w.stat.LastTry = time.Now()
	w.stat.LastOK = err == nil
	if err != nil {
		w.stat.LastErr = err.Error()
	} else {
		w.stat.LastErr = ""
	}
	w.mu.Unlock()

	return err
}

func (w *Webhook) post(ctx context.Context, ev store.Event) error {
	target, ctype, body, err := buildRequest(w.cfg, ev, time.Now())
	if err != nil {
		return err
	}
	req, err := httpx.NewRequest(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	// 显式设置长度：部分网关对 chunked 请求体不友好。
	req.ContentLength = int64(len(body))

	resp, err := w.client.Do(req)
	if err != nil {
		// 错误里可能带上完整 URL（Go 的 *url.Error 就会），而 URL 含密钥。
		return maskErr(err, w.cfg.URL)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, trim(string(raw), 200))
	}
	return checkResponse(w.cfg.Kind, raw)
}

// Close 停止收新事件，把队列里积压的发完，最多等 timeout。
func (w *Webhook) Close(timeout time.Duration) {
	w.once.Do(func() { close(w.queue) })
	select {
	case <-w.done:
	case <-time.After(timeout):
		w.log.Warn("退出时仍有通知未发完", "渠道", w.cfg.Name)
	}
}

// MaskURL 把 webhook 地址脱敏成「host + 密钥尾 4 位」。
//
// webhook URL 里的 key/token 就是密钥：日志会落盘、/api/notify 对整个局域网
// 可见，完整 URL 泄露出去等于把发消息的权限交出去。
func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(地址无效)"
	}
	out := u.Host
	// 密钥通常在 query（企微 key、钉钉 access_token）或路径末段（飞书 hook id）。
	secret := ""
	for _, k := range []string{"key", "access_token", "token"} {
		if v := u.Query().Get(k); v != "" {
			secret = v
			break
		}
	}
	if secret == "" && u.Path != "" {
		if i := lastSeg(u.Path); i != "" {
			secret = i
		}
	}
	if secret != "" {
		out += "/…" + tail(secret, 4)
	}
	return out
}

// maskErr 把错误文本里出现的完整 URL 替换成脱敏形式。
func maskErr(err error, raw string) error {
	s := err.Error()
	if raw == "" {
		return err
	}
	if masked := strings.ReplaceAll(s, raw, MaskURL(raw)); masked != s {
		return fmt.Errorf("%s", masked)
	}
	return err
}
