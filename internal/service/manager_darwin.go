//go:build darwin

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type darwinManager struct{ log *slog.Logger }

func newManager(log *slog.Logger) (Manager, error) { return &darwinManager{log: log}, nil }

func requirePrivilege() error { return nil } // LaunchAgent 装在自己家目录，不需要 sudo

// domain 与 target 是 launchctl 的两种寻址方式。
// 统一用 gui/$UID：作业落在图形会话里，桌面通知才有去处。
func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }
func target() string { return domain() + "/" + DarwinLabel }

// run 执行 launchctl 并返回合并后的输出。
func (m *darwinManager) run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s != "" {
			return s, fmt.Errorf("launchctl %s: %w (%s)", strings.Join(args, " "), err, s)
		}
		return s, fmt.Errorf("launchctl %s: %w", strings.Join(args, " "), err)
	}
	return s, nil
}

func (m *darwinManager) Install(ctx context.Context, s Spec) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	data, err := renderPlist(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败: %w", err)
	}
	// launchd 会拒绝 group/world 可写的 plist，权限必须是 0644。
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}

	// 重复 bootstrap 会报 "Bootstrap failed: 5: Input/output error"，
	// 所以先无条件 bootout 一次，忽略它的错误（未装载时必然失败）。
	m.run(ctx, "bootout", target())

	if _, err := m.run(ctx, "bootstrap", domain(), path); err != nil {
		os.Remove(path)
		return fmt.Errorf("装载服务失败: %w", err)
	}
	return nil
}

func (m *darwinManager) Uninstall(ctx context.Context) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	m.run(ctx, "bootout", target()) // 未装载时报错是正常的
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 %s 失败: %w", path, err)
	}
	return nil
}

func (m *darwinManager) Start(ctx context.Context) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("服务尚未安装，先执行 isp-probe service install")
	}
	// 已装载时 bootstrap 会失败，此时用 kickstart 把作业拉起来。
	if _, err := m.run(ctx, "bootstrap", domain(), path); err == nil {
		return nil
	}
	_, err = m.run(ctx, "kickstart", "-k", target())
	return err
}

// Stop 用 bootout 而不是 launchctl stop。
//
// stop 会被 KeepAlive 立刻把进程拉回来；bootout 只把作业从当前会话卸下，
// plist 仍留在盘上 —— 「本次停掉、下次登录照旧自启」正是需要的语义。
func (m *darwinManager) Stop(ctx context.Context) error {
	if _, err := m.run(ctx, "bootout", target()); err != nil {
		return fmt.Errorf("停止服务失败（可能本来就没在运行）: %w", err)
	}
	return nil
}

var (
	reState = regexp.MustCompile(`(?m)^\s*state\s*=\s*(\S+)`)
	rePID   = regexp.MustCompile(`(?m)^\s*pid\s*=\s*(\d+)`)
	reArgs  = regexp.MustCompile(`(?s)arguments\s*=\s*\{(.*?)\n\t*\}`)
)

// Status 合成三重证据，而不是只信 launchctl。
//
// launchctl print 的输出格式跨 macOS 版本会变，正则解析是脆的；所以
// 「plist 是否存在」决定 Installed，「print 的退出码」决定是否已装载，
// 而调用方另外用 HTTP 探活作为「是否真的在跑」的权威判据。
// 解析失败时降级为「已装载但状态未知」，不报错。
func (m *darwinManager) Status(ctx context.Context) (Status, error) {
	var st Status
	path, err := plistPath()
	if err != nil {
		return st, err
	}
	if _, err := os.Stat(path); err != nil {
		st.State = "未安装"
		return st, nil
	}
	st.Installed = true
	st.State = "已停止"

	out, err := m.run(ctx, "print", target())
	if err != nil {
		return st, nil // 未装载
	}
	st.Detail = firstLines(out, 3)

	if mm := reState.FindStringSubmatch(out); mm != nil {
		if mm[1] == "running" {
			st.Running, st.State = true, "运行中"
		} else {
			st.State = mm[1]
		}
	} else {
		st.State = "已装载（状态未知）"
	}
	if mm := rePID.FindStringSubmatch(out); mm != nil {
		st.PID, _ = strconv.Atoi(mm[1])
		if st.PID > 0 {
			st.Running, st.State = true, "运行中"
		}
	}
	st.ExePath, st.CfgPath = parseArgs(out)
	return st, nil
}

// parseArgs 从 launchctl print 的 arguments 块里取出程序路径与 -c 的值。
func parseArgs(out string) (exe, cfg string) {
	mm := reArgs.FindStringSubmatch(out)
	if mm == nil {
		return "", ""
	}
	var args []string
	for _, line := range strings.Split(mm[1], "\n") {
		if s := strings.TrimSpace(line); s != "" {
			args = append(args, s)
		}
	}
	if len(args) > 0 {
		exe = args[0]
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" {
			cfg = args[i+1]
		}
	}
	return exe, cfg
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}
