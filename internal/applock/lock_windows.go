//go:build windows

package applock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type Lock struct{ h windows.Handle }

func acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建锁文件目录失败: %w", err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// dwShareMode = 0 表示完全独占：第二个进程打开同一个文件会拿到
	// ERROR_SHARING_VIOLATION。句柄随进程结束由内核关闭，锁自动释放。
	// FILE_FLAG_DELETE_ON_CLOSE 让锁文件不残留在磁盘上。
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("打开锁文件 %s 失败: %w", path, err)
	}

	// 写入 PID 纯粹是为了人工排查，判定冲突不依赖它。
	var n uint32
	buf := []byte(fmt.Sprintf("%d\n", os.Getpid()))
	windows.WriteFile(h, buf, &n, nil)
	return &Lock{h: h}, nil
}

func (l *Lock) release() error {
	if l == nil || l.h == 0 {
		return nil
	}
	h := l.h
	l.h = 0
	return windows.CloseHandle(h)
}
