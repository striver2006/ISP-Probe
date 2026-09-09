package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func baseYAML() string {
	return `
links:
  - id: telecom
    name: "电信"
    modem_ip: "192.168.10.1"
probe:
  anchors: ["223.5.5.5:443"]
`
}

func writeCfg(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	return p
}

// 模板写错是配置问题，必须在启动时报出来 —— 而不是等线路真断了、
// 最需要告警的那一刻才发现发不出去。
func TestWebhookValidation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string // 空 = 应通过
	}{
		{
			name:    "kind 非法",
			yaml:    "notify:\n  webhooks:\n    - kind: telegram\n      url: https://x.com/h\n",
			wantErr: "kind",
		},
		{
			name:    "url 缺失",
			yaml:    "notify:\n  webhooks:\n    - kind: wecom\n      url: \"\"\n",
			wantErr: "url",
		},
		{
			name:    "url 非 http",
			yaml:    "notify:\n  webhooks:\n    - kind: wecom\n      url: ftp://x.com/h\n",
			wantErr: "url",
		},
		{
			name:    "custom 缺模板",
			yaml:    "notify:\n  webhooks:\n    - kind: custom\n      url: https://x.com/h\n",
			wantErr: "body_template",
		},
		{
			name:    "custom 模板语法错",
			yaml:    "notify:\n  webhooks:\n    - kind: custom\n      url: https://x.com/h\n      body_template: \"{{.Kind\"\n",
			wantErr: "body_template",
		},
		{
			name: "合法配置",
			yaml: "notify:\n  webhooks:\n    - name: 群\n      kind: wecom\n      url: https://x.com/h\n",
		},
	}

	for _, c := range cases {
		dir := t.TempDir()
		p := writeCfg(t, dir, "config.yaml", baseYAML()+c.yaml)
		_, err := Load(p)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: 应通过校验，实得 %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 应报错但通过了", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: 错误信息应提到 %q，实得 %v", c.name, c.wantErr, err)
		}
	}
}

func TestWebhookDefaults(t *testing.T) {
	dir := t.TempDir()
	p := writeCfg(t, dir, "config.yaml", baseYAML()+
		"notify:\n  webhooks:\n    - kind: wecom\n      url: https://x.com/h\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	w := cfg.Notify.Webhooks[0]
	if w.Name != "wecom" {
		t.Errorf("name 留空应回填 kind，实得 %q", w.Name)
	}
	if w.Timeout != 8*time.Second {
		t.Errorf("timeout 应回落默认 8s，实得 %v", w.Timeout)
	}
	if !cfg.Notify.RepeatAlert {
		t.Error("repeat_alert 默认应为 true")
	}
}

// 旁路配置的存在理由：config.yaml 随仓库提交，而 webhook URL 里的 key 是密钥。
func TestLocalOverride(t *testing.T) {
	dir := t.TempDir()
	p := writeCfg(t, dir, "config.yaml", baseYAML()+
		"notify:\n  webhooks:\n    - name: 占位\n      kind: wecom\n      url: https://placeholder/h\n")

	// 不存在旁路文件是常态，不能报错。
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("无旁路文件时应正常加载，实得 %v", err)
	}
	if cfg.Notify.Webhooks[0].URL != "https://placeholder/h" {
		t.Error("无旁路文件时应保留主配置的值")
	}

	// 旁路覆盖生效。
	writeCfg(t, dir, LocalName,
		"notify:\n  webhooks:\n    - name: 真机器人\n      kind: wecom\n      url: https://real/h?key=secret\n      enabled: true\nweb:\n  token: t0ken\n")
	cfg, err = Load(p)
	if err != nil {
		t.Fatalf("加载旁路配置失败: %v", err)
	}
	if len(cfg.Notify.Webhooks) != 1 || cfg.Notify.Webhooks[0].Name != "真机器人" {
		t.Errorf("旁路里的 webhooks 应整体替换主配置那份，实得 %+v", cfg.Notify.Webhooks)
	}
	if cfg.Web.Token != "t0ken" {
		t.Errorf("旁路应能覆盖 web.token，实得 %q", cfg.Web.Token)
	}
	// 旁路没提到的字段保持不变。
	if len(cfg.Links) != 1 || cfg.Links[0].ID != "telecom" {
		t.Error("旁路未提及的字段应原样保留")
	}

	// 存在却解析不了必须报错：静默忽略会让人以为密钥已生效，实际一条也发不出去。
	writeCfg(t, dir, LocalName, "notify:\n  webhooks: [oops\n")
	if _, err := Load(p); err == nil {
		t.Error("旁路配置解析失败时必须报错")
	} else if !strings.Contains(err.Error(), LocalName) {
		t.Errorf("错误信息应指明是旁路文件，实得 %v", err)
	}
}
