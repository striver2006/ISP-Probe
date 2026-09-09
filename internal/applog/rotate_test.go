package applog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestRotateKeepsBounded 验证轮转会切分文件并把历史文件数量控制在 keep 以内。
func TestRotateKeepsBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	r := newRotator(path, 100, 2) // 100 字节一切，留 2 个历史
	defer r.Close()

	line := make([]byte, 40)
	for i := range line {
		line[i] = 'x'
	}
	line[len(line)-1] = '\n'
	for i := 0; i < 20; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("当前日志文件应存在: %v", err)
	}
	for _, n := range []int{1, 2} {
		if _, err := os.Stat(fmt.Sprintf("%s.%d", path, n)); err != nil {
			t.Errorf("历史文件 .%d 应存在: %v", n, err)
		}
	}
	// keep=2 意味着 .3 必须被丢弃，否则日志会无限增长。
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error(".3 不该存在：超出 keep 的历史文件必须被丢弃")
	}
}

// TestWriteKeepsRecordIntact 验证单条记录不会被切到两个文件里。
// slog 每条日志只调一次 Write，轮转判断必须整条进行。
func TestWriteKeepsRecordIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	r := newRotator(path, 50, 3)
	defer r.Close()

	first := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n") // 41 字节
	second := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n")
	r.Write(first)
	r.Write(second) // 41+41 > 50，必须先轮转

	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != string(second) {
		t.Errorf("当前文件应完整含第二条记录，实得 %q", cur)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != string(first) {
		t.Errorf("历史文件应完整含第一条记录，实得 %q", old)
	}
}

// TestNewSurfacesUnwritableDir 验证目录不可写时在构造阶段就报错，
// 而不是等到第一条日志才静默失败 —— 服务模式下那等于什么都没记。
func TestNewSurfacesUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(Options{File: filepath.Join(blocker, "sub", "app.log")}); err == nil {
		t.Error("日志目录不可创建时 New 应返回错误")
	}
}
