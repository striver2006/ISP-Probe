//go:build darwin

package applock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Lock struct{ f *os.File }

func acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建锁文件目录失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件 %s 失败: %w", path, err)
	}
	// LOCK_NB 让冲突立刻返回而不是阻塞等待。锁随 fd 关闭或进程结束自动释放。
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %s", ErrLocked, path)
	}
	// 写入 PID 纯粹是为了人工排查（lsof / cat 都能看），判定冲突不依赖它。
	f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Sync()
	return &Lock{f: f}, nil
}

func (l *Lock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}
