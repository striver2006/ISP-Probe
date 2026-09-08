// Package notify 负责把线路状态变更推送为系统桌面通知。
package notify

import (
	"sync"
	"time"

	"github.com/gen2brain/beeep"
)

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

func (d *Desktop) Notify(title, message string) error {
	d.mu.Lock()
	if t, ok := d.last[message]; ok && time.Since(t) < d.minGap {
		d.mu.Unlock()
		return nil
	}
	d.last[message] = time.Now()
	d.mu.Unlock()

	return beeep.Notify(title, message, "")
}

// Discard 是不发通知的实现，用于 --no-notify 或测试。
type Discard struct{}

func (Discard) Notify(string, string) error { return nil }
