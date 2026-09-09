//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type winManager struct{ log *slog.Logger }

func newManager(log *slog.Logger) (Manager, error) { return &winManager{log: log}, nil }

// requirePrivilege 先检测再动手，避免用户看到裸的 "Access is denied."。
func requirePrivilege() error {
	if windows.GetCurrentProcessToken().IsElevated() {
		return nil
	}
	return errors.New("安装/卸载/启停 Windows 服务需要管理员权限。\n" +
		"  请用「以管理员身份运行」打开 PowerShell 或终端后重试。")
}

func (m *winManager) Install(ctx context.Context, sp Spec) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败: %w", err)
	}
	defer scm.Disconnect()

	if s, err := scm.OpenService(WindowsName); err == nil {
		s.Close()
		return fmt.Errorf("服务 %s 已存在，用 --force 覆盖或先执行 service uninstall", WindowsName)
	}

	// 参数交给 CreateService 的可变参数拼装，不自己拼 ImagePath 字符串 ——
	// 安装路径含空格是常态，手工加引号极易出错。
	s, err := scm.CreateService(WindowsName, sp.Exe, mgr.Config{
		DisplayName:  WindowsDisplayName,
		Description:  WindowsDescription,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		Dependencies: []string{"Tcpip"},
		// 延迟自启：开机瞬间网络栈与物理网卡尚未就绪，iface.Detect 会失败
		// 或选错网卡；对每分钟一次的探测来说，晚几十秒毫无损失。
		DelayedAutoStart: true,
	}, "serve", "-c", sp.Cfg)
	if err != nil {
		return fmt.Errorf("创建服务失败: %w", err)
	}
	defer s.Close()

	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400); err != nil {
		m.log.Warn("设置失败重启策略失败（不影响服务本身）", "err", err)
	}
	// 端口被占、配置出错这类非崩溃退出也要触发重启策略，
	// 否则服务只会一声不响地停在那里。
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		m.log.Warn("设置非崩溃失败重启失败（不影响服务本身）", "err", err)
	}
	return nil
}

func (m *winManager) Uninstall(ctx context.Context) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败: %w", err)
	}
	defer scm.Disconnect()

	s, err := scm.OpenService(WindowsName)
	if err != nil {
		return fmt.Errorf("服务 %s 未安装", WindowsName)
	}
	defer s.Close()

	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			m.log.Warn("发送停止指令失败，仍将继续删除", "err", err)
		}
		waitState(ctx, s, svc.Stopped, 20*time.Second)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("删除服务失败: %w", err)
	}
	return nil
}

func (m *winManager) Start(ctx context.Context) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败: %w", err)
	}
	defer scm.Disconnect()

	s, err := scm.OpenService(WindowsName)
	if err != nil {
		return fmt.Errorf("服务尚未安装，先执行 isp-probe service install")
	}
	defer s.Close()

	if st, err := s.Query(); err == nil && st.State == svc.Running {
		return nil
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("启动服务失败: %w", err)
	}
	return nil
}

func (m *winManager) Stop(ctx context.Context) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败: %w", err)
	}
	defer scm.Disconnect()

	s, err := scm.OpenService(WindowsName)
	if err != nil {
		return fmt.Errorf("服务 %s 未安装", WindowsName)
	}
	defer s.Close()

	if st, err := s.Query(); err == nil && st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("停止服务失败: %w", err)
	}
	waitState(ctx, s, svc.Stopped, 20*time.Second)
	return nil
}

// Status 走只读路径，不要求管理员权限：SC_MANAGER_CONNECT 对普通用户开放，
// 查询用的 SERVICE_QUERY_STATUS|SERVICE_QUERY_CONFIG 也是。
func (m *winManager) Status(ctx context.Context) (Status, error) {
	var out Status
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return out, fmt.Errorf("连接服务控制管理器失败: %w", err)
	}
	scm := &mgr.Mgr{Handle: h}
	defer scm.Disconnect()

	name, err := windows.UTF16PtrFromString(WindowsName)
	if err != nil {
		return out, err
	}
	sh, err := windows.OpenService(scm.Handle, name,
		windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		out.State = "未安装"
		return out, nil
	}
	s := &mgr.Service{Name: WindowsName, Handle: sh}
	defer s.Close()

	out.Installed = true
	out.State = "已停止"

	if st, err := s.Query(); err == nil {
		out.PID = int(st.ProcessId)
		switch st.State {
		case svc.Running:
			out.Running, out.State = true, "运行中"
		case svc.StartPending:
			out.State = "启动中"
		case svc.StopPending:
			out.State = "停止中"
		case svc.Stopped:
			out.State = "已停止"
		default:
			out.State = fmt.Sprintf("状态码 %d", st.State)
		}
	}
	if c, err := s.Config(); err == nil {
		out.ExePath, out.CfgPath = parseImagePath(c.BinaryPathName)
		out.Detail = c.BinaryPathName
	}
	return out, nil
}

// parseImagePath 从 ImagePath 里还原可执行文件路径与 -c 的值。
// CreateService 给含空格的部分加了引号，这里按引号规则拆回来。
func parseImagePath(s string) (exe, cfg string) {
	var args []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
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

// waitState 轮询直到服务进入目标状态或超时。超时不算错误 ——
// 调用方随后的操作（Delete / 打印状态）本身会暴露真实结果。
func waitState(ctx context.Context, s *mgr.Service, want svc.State, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil || st.State == want {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}
