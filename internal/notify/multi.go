package notify

import (
	"context"
	"log/slog"
	"time"

	"isp-probe/internal/store"
)

// Multi 把一条事件扇出到多个渠道。
//
// 单个渠道失败只记日志，不影响其余渠道 —— 桌面通知在 Windows 服务模式下
// 本来就发不出去，不能让它把 webhook 一起拖垮。
type Multi struct {
	chans []Channel
	log   *slog.Logger
}

func NewMulti(log *slog.Logger, cs ...Channel) *Multi {
	return &Multi{chans: cs, log: log}
}

// Channels 暴露底层渠道，供面板与 notify test 逐个操作。
func (m *Multi) Channels() []Channel { return m.chans }

func (m *Multi) Name() string { return "multi" }

func (m *Multi) Notify(ctx context.Context, ev store.Event) error {
	for _, c := range m.chans {
		if err := c.Notify(ctx, ev); err != nil {
			m.log.Warn("发送通知失败", "渠道", c.Name(), "err", err)
		}
	}
	// 恒返回 nil：调用方（状态机）对单个渠道的成败无能为力，
	// 失败已经记进日志，也能在面板的通知卡片上看到。
	return nil
}

// Close 逐个关闭渠道，总耗时不超过 timeout。
func (m *Multi) Close(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for _, c := range m.chans {
		left := time.Until(deadline)
		if left <= 0 {
			return
		}
		c.Close(left)
	}
}
