// Package applog 构造诊断日志的 slog.Logger。
//
// 前台运行输出到 stderr（与服务化之前完全一致），服务模式输出到会轮转的
// 文件 —— 且只输出到文件，不再同时写 stderr：Windows 服务的 stderr 落到
// 虚空，launchd 的 stderr 已被 plist 重定向到另一个文件，两边都写就是双份。
package applog

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

type Options struct {
	Level   slog.Level
	File    string // 留空则只写 stderr
	MaxSize int    // MB，<=0 取 8
	Keep    int    // 保留的历史文件数，<=0 取 3
}

// New 返回 logger 与一个必须在进程退出前调用的关闭函数。
func New(o Options) (*slog.Logger, func(), error) {
	if o.File == "" {
		h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: o.Level})
		return slog.New(h), func() {}, nil
	}

	abs, err := filepath.Abs(o.File)
	if err != nil {
		return nil, nil, err
	}
	if o.MaxSize <= 0 {
		o.MaxSize = 8
	}
	if o.Keep <= 0 {
		o.Keep = 3
	}
	w := newRotator(abs, int64(o.MaxSize)<<20, o.Keep)
	if err := w.ensure(); err != nil {
		return nil, nil, err
	}

	h := slog.NewTextHandler(io.Writer(w), &slog.HandlerOptions{Level: o.Level})
	return slog.New(h), func() { w.Close() }, nil
}
