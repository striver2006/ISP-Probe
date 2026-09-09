package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const minimal = `
links:
  - id: telecom
    name: 电信
    modem_ip: 192.168.2.1
  - id: mobile
    name: 移动
    modem_ip: 192.168.1.1
`

// TestLoadAnchorsRelativePaths 是服务模式能用的前提：
// launchd / SCM 下 cwd 是 / 或 System32，相对路径若不锚定就会落到那里。
func TestLoadAnchorsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(minimal+"\nstore:\n  path: data.db\nlog:\n  file: logs/a.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "data.db"); cfg.Store.Path != want {
		t.Errorf("store.path 应锚定为 %s，实得 %s", want, cfg.Store.Path)
	}
	if want := filepath.Join(dir, "logs", "a.log"); cfg.Log.File != want {
		t.Errorf("log.file 应锚定为 %s，实得 %s", want, cfg.Log.File)
	}
}

// TestLoadKeepsAbsolutePaths 验证已是绝对路径的值不被改动。
func TestLoadKeepsAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(t.TempDir(), "elsewhere.db")
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(minimal+"\nstore:\n  path: "+abs+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Path != abs {
		t.Errorf("绝对路径不应被改动，want %s got %s", abs, cfg.Store.Path)
	}
}

// TestLoadMissingFileIsDistinct 保证「文件不存在」不再伪装成
// 「配置中未定义任何线路」—— 后者会把人引向完全错误的方向。
func TestLoadMissingFileIsDistinct(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("应返回 ErrConfigNotFound，实得 %v", err)
	}
}

// TestLogDefaults 验证轮转参数有兜底值，避免 0 值导致每条日志都轮转。
func TestLogDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.MaxSizeMB <= 0 || cfg.Log.Keep <= 0 {
		t.Errorf("日志轮转参数应有默认值，实得 MaxSizeMB=%d Keep=%d", cfg.Log.MaxSizeMB, cfg.Log.Keep)
	}
}
