package applog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rotator 是最小可用的按大小轮转写入器。
//
// 只做 size-based，不做按时间切割：探针每分钟产生个位数条日志，8MB 能装
// 很久，按时间查询用 grep 时间戳就够，不值得为此引入 lumberjack 依赖。
type rotator struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	keep    int
	f       *os.File
	size    int64
}

func newRotator(path string, maxSize int64, keep int) *rotator {
	return &rotator{path: path, maxSize: maxSize, keep: keep}
}

func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	// slog 的 TextHandler 每条记录只调一次 Write，所以在这里整条判断
	// 就能保证一条日志不会被切到两个文件里。
	if r.size > 0 && r.size+int64(len(p)) > r.maxSize {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// ensure 提前打开日志文件，让「目录不可建 / 文件不可写」这类错误在启动时
// 就暴露出来。留到第一条日志才发现的话，服务会静默地什么都不记 ——
// 那恰恰是最需要日志的场景。
func (r *rotator) ensure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		return nil
	}
	return r.open()
}

func (r *rotator) open() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	size := int64(0)
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	r.f, r.size = f, size
	return nil
}

// rotate 把 .log → .log.1，.log.1 → .log.2 …，丢弃最旧的一个。
//
// 顺序是从旧到新逐个改名，且先删掉最旧的那个：Windows 上 os.Rename
// 覆盖已存在的目标会失败，必须保证每次改名的目标都不存在。
func (r *rotator) rotate() error {
	r.f.Close()
	r.f, r.size = nil, 0

	os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	os.Rename(r.path, r.path+".1")
	return r.open()
}

func (r *rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
