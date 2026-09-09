//go:build darwin

package service

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"text/template"
)

// plistPath 返回 LaunchAgent 的安装位置。
//
// 用 LaunchAgent（~/Library/LaunchAgents）而非 LaunchDaemon：作业跑在
// 用户的图形会话里，桌面通知才能送达。代价是必须登录后才启动。
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", DarwinLabel+".plist"), nil
}

// x 转义要嵌进 plist 的字符串。路径里出现 & 会让 plist 解析失败，
// 而 launchd 对此的报错极其晦涩，所以宁可无条件转义。
func x(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

var plistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>

	<key>ProgramArguments</key>
	<array>
		<string>{{.Exe}}</string>
		<string>serve</string>
		<string>-c</string>
		<string>{{.Cfg}}</string>
	</array>

	<!-- 固定工作目录：即使 -c 参数将来丢了，相对路径回退也仍落在正确位置 -->
	<key>WorkingDirectory</key>
	<string>{{.WorkDir}}</string>

	<!-- 登录即启动 -->
	<key>RunAtLoad</key>
	<true/>

	<!-- 只在非正常退出时重启。收到 SIGTERM 后我们优雅退出并返回 0，
	     这样 service stop 不会被 KeepAlive 立刻拉回来。 -->
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>

	<!-- 崩溃循环时的最小重启间隔，防止刷屏 -->
	<key>ThrottleInterval</key>
	<integer>10</integer>

	<!-- 后台服务：降低调度与 I/O 优先级，不与用户交互任务抢资源 -->
	<key>ProcessType</key>
	<string>Background</string>

	<!-- 这两个只兜底捕获 Go runtime panic 一类绕过 slog 的输出；
	     正式日志由程序自己写 logs/isp-probe.log 并轮转，正常情况下这两个文件是空的。 -->
	<key>StandardOutPath</key>
	<string>{{.OutLog}}</string>
	<key>StandardErrorPath</key>
	<string>{{.ErrLog}}</string>

	<key>EnvironmentVariables</key>
	<dict>
		<key>{{.EnvKey}}</key>
		<string>launchd</string>
	</dict>

	<!-- 限定在图形登录会话，桌面通知才能到达用户 -->
	<key>LimitLoadToSessionType</key>
	<string>Aqua</string>
</dict>
</plist>
`))

func renderPlist(s Spec) ([]byte, error) {
	data := struct{ Label, Exe, Cfg, WorkDir, OutLog, ErrLog, EnvKey string }{
		Label:   x(DarwinLabel),
		Exe:     x(s.Exe),
		Cfg:     x(s.Cfg),
		WorkDir: x(s.WorkDir),
		OutLog:  x(filepath.Join(s.LogDir, "launchd.out.log")),
		ErrLog:  x(filepath.Join(s.LogDir, "launchd.err.log")),
		EnvKey:  x(EnvManaged),
	}
	var b bytes.Buffer
	if err := plistTmpl.Execute(&b, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
