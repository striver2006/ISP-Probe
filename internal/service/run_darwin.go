//go:build darwin

package service

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// isManaged 靠 plist 注入的环境变量判断。macOS 没有类似
// svc.IsWindowsService() 的内核级判据，环境变量是最直接可靠的方式。
func isManaged() bool { return os.Getenv(EnvManaged) != "" }

// runManaged 在 darwin 上很薄：launchd 用 SIGTERM 停止作业，
// 现有的信号处理已经足够，也不需要 ready 回调。
func runManaged(fn Runner) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return fn(ctx, func() {})
}

func dirOf(p string) string { return filepath.Dir(p) }
