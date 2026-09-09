// Package notify 负责把线路状态变更推送出去：系统桌面通知与 IM 群机器人。
package notify

import (
	"context"
	"sync"
	"time"

	"isp-probe/internal/store"

	"github.com/gen2brain/beeep"
)

// Channel 是一个通知渠道。
//
// 传整个 store.Event 而不是 (title, message) 两个字符串：IM 渠道需要按 Kind
// 过滤、区分展示样式，只给拼好的文案就什么都做不了。
type Channel interface {
	// Name 是渠道在日志与面板上的显示名。
	Name() string
	// Notify 投递一条事件。实现可以是异步的（见 Webhook），此时返回 nil
	// 只代表「已受理」，不代表已送达。
	Notify(ctx context.Context, ev store.Event) error
	// Close 在进程退出前调用，给异步实现一个把积压发完的机会。
	Close(timeout time.Duration)
}

// Title 把事件类型翻译成通知标题。
func Title(kind string) string {
	switch kind {
	case "up":
		return "ISP探针 · 线路恢复"
	case "down_repeat":
		return "ISP探针 · 线路仍未恢复"
	default:
		return "ISP探针 · 线路故障"
	}
}

// Desktop 发送系统桌面通知（macOS 走 osascript，Windows 走 WinRT Toast）。
//
// 内置最小间隔限流：状态机层面已用连续失败阈值抑制了抖动，这里再兜一层，
// 避免极端情况下（例如线路反复闪断）把通知中心刷屏。
type Desktop struct {
	minGap time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

func NewDesktop(minGap time.Duration) *Desktop {
	if minGap <= 0 {
		minGap = 30 * time.Second
	}
	return &Desktop{minGap: minGap, last: make(map[string]time.Time)}
}

func (d *Desktop) Name() string { return "桌面通知" }

func (d *Desktop) Notify(_ context.Context, ev store.Event) error {
	// 限流 key 必须带上 Message：持续故障的重复提醒 Kind 与 LinkID 都相同，
	// 只靠这两项会被这里当成重复而吞掉，正好把「仍未恢复」的提醒全灭了。
	// Message 里含已持续分钟数，天然互不相同。
	key := ev.LinkID + "|" + ev.Kind + "|" + ev.Message

	d.mu.Lock()
	if t, ok := d.last[key]; ok && time.Since(t) < d.minGap {
		d.mu.Unlock()
		return nil
	}
	d.last[key] = time.Now()
	d.mu.Unlock()

	return beeep.Notify(Title(ev.Kind), ev.Message, "")
}

func (d *Desktop) Close(time.Duration) {}

// Discard 是不发通知的实现，用于一次性命令与测试。
type Discard struct{}

func (Discard) Name() string                              { return "discard" }
func (Discard) Notify(context.Context, store.Event) error { return nil }
func (Discard) Close(time.Duration)                       {}
