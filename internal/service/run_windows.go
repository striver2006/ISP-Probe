//go:build windows

package service

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
)

func isManaged() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

func runManaged(fn Runner) error { return svc.Run(WindowsName, &winHandler{fn: fn}) }

func dirOf(p string) string { return filepath.Dir(p) }

type winHandler struct{ fn Runner }

// Execute 是 SCM 的调度循环。
//
// 关键约束：svc.Run 必须尽早被调用，且在 ready 之前要持续上报 StartPending
// 心跳 —— 配置加载、网卡探测、开库串起来可能几秒，期间若一声不响，SCM 会
// 认为服务卡死并判定启动失败。所以 ready 之前绝不能有无心跳的阻塞调用。
func (h *winHandler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	const waitHint = 20000 // ms，要比业务侧的优雅退出预算宽

	s <- svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: waitHint}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	go func() { done <- h.fn(ctx, func() { once.Do(func() { close(ready) }) }) }()

	beat := time.NewTicker(2 * time.Second)
	defer beat.Stop()
	cp := uint32(1)

init:
	for {
		select {
		case <-ready:
			break init
		case err := <-done:
			// 还没 ready 就退出了 —— 配置错误、端口被占之类的启动失败。
			return false, exitCode(err)
		case <-beat.C:
			cp++
			s <- svc.Status{State: svc.StartPending, CheckPoint: cp, WaitHint: waitHint}
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				cancel()
				return false, exitCode(<-done)
			}
		}
	}
	beat.Stop()

	s <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending, WaitHint: waitHint}
				cancel()
				select {
				case err := <-done:
					return false, exitCode(err)
				case <-time.After(15 * time.Second):
					return false, 2 // 超时未退，让 SCM 强杀
				}
			}
		case err := <-done:
			// 运行中自行退出（面板挂了等）。返回非 0 触发失败重启策略。
			return false, exitCode(err)
		}
	}
}

func exitCode(err error) uint32 {
	if err != nil {
		return 1
	}
	return 0
}
