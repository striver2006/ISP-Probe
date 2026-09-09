package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"isp-probe/internal/apppath"
	"isp-probe/internal/config"
	"isp-probe/internal/doctor"
	"isp-probe/internal/service"
)

const serviceUsage = `isp-probe service <子命令> [选项]

子命令:
  install     安装为随登录自启的后台服务并启动
  uninstall   停止并卸载
  start       启动已安装的服务
  stop        停止（不卸载，下次登录仍会自启）
  status      查看安装与运行状态
  logs        打印服务日志尾部

install 选项:
  -c <path>      配置文件路径（会转成绝对路径写进服务定义）
  --log-dir <d>  日志目录（默认 <配置目录>/logs）
  --no-start     只安装不启动
  --skip-doctor  跳过安装前的环境自检
  --force        已安装时先卸载再装

logs 选项:
  -n <行数>      默认 50。持续跟踪请用 tail -f / Get-Content -Wait
`

// runServiceCmd 处理 service 子命令组，返回进程退出码。
//
// 除 install 外，这里的子命令都不加载配置、也不探测网卡：那两步任一失败
// 都会让 status / stop / uninstall 无法使用，而它们恰恰是出问题时要用的。
func runServiceCmd(args []string) int {
	if len(args) == 0 {
		fmt.Print(serviceUsage)
		return 2
	}
	sub := args[0]
	rest := args[1:]

	// 服务管理命令的输出是给人看的，日志级别固定 Info、写 stderr。
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mgr, err := service.New(log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ctx, cancel := signalCtx()
	defer cancel()

	switch sub {
	case "install":
		err = serviceInstall(ctx, mgr, rest)
	case "uninstall":
		err = serviceUninstall(ctx, mgr)
	case "start":
		err = serviceStart(ctx, mgr)
	case "stop":
		err = serviceStop(ctx, mgr)
	case "status":
		err = serviceStatus(ctx, mgr)
	case "logs":
		err = serviceLogs(ctx, mgr, rest)
	case "-h", "--help", "help":
		fmt.Print(serviceUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: service %s\n\n", sub)
		fmt.Print(serviceUsage)
		return 2
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func serviceInstall(ctx context.Context, mgr service.Manager, args []string) error {
	fs := flag.NewFlagSet("service install", flag.ExitOnError)
	var o opts
	bindGlobalFlags(fs, &o)
	logDir := fs.String("log-dir", "", "日志目录")
	noStart := fs.Bool("no-start", false, "只安装不启动")
	skipDoctor := fs.Bool("skip-doctor", false, "跳过安装前自检")
	force := fs.Bool("force", false, "已安装时先卸载再装")
	_ = fs.Parse(args)

	paths := apppath.Discover()
	// go run 产生的二进制退出即删，把它的路径写进服务定义必然失效。
	if paths.IsTempBuild() {
		return fmt.Errorf("当前可执行文件在临时目录 (%s)，看起来是 go run 跑起来的。\n"+
			"  先 go build -o isp-probe ./cmd/isp-probe，再用编译出的二进制安装。", paths.ExeDir)
	}
	if paths.Exe == "" {
		return fmt.Errorf("无法确定可执行文件路径")
	}

	cfgFile, err := paths.FindConfig(o.cfgPath)
	if err != nil {
		return err
	}
	// 先校验配置：装一个启动就会失败的服务，比不装更糟。
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("配置错误: %w", err)
	}

	if err := service.RequirePrivilege(); err != nil {
		return err
	}
	if err := warnSSH(*force); err != nil {
		return err
	}

	// 默认先跑一遍自检。TUN 绕过没生效时程序不会报错，只会安静地产出假
	// 数据 —— 一个后台常驻、无人盯着的服务产出假数据，比前台跑更危险。
	if !*skipDoctor {
		fmt.Println("正在做安装前自检…")
		p, b, err := setupIface(cfg)
		if err != nil {
			return err
		}
		rep := doctor.Run(ctx, cfg, p, b)
		for _, c := range rep.Checks {
			if c.Level == doctor.LevelFail {
				fmt.Printf("  ✗ %s\n     %s\n", c.Name, c.Detail)
			}
		}
		if rep.HasFailure() {
			return fmt.Errorf("自检未通过，安装中止。\n" +
				"  先运行 isp-probe doctor 处理上面的问题，或用 --skip-doctor 强行安装。")
		}
		fmt.Println("  ✓ 自检通过")
	}

	if st, err := mgr.Status(ctx); err == nil && st.Installed {
		if !*force {
			return fmt.Errorf("服务已安装（%s）。用 --force 覆盖，或先执行 isp-probe service uninstall", st.State)
		}
		if err := mgr.Uninstall(ctx); err != nil {
			return fmt.Errorf("覆盖安装前的卸载失败: %w", err)
		}
	}

	base := filepath.Dir(cfgFile)
	dir := *logDir
	if dir == "" {
		dir = filepath.Join(base, "logs")
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}

	spec := service.Spec{
		Exe: paths.Exe, Cfg: cfgFile, WorkDir: base,
		LogDir: dir, Listen: cfg.Web.Listen,
	}
	if err := mgr.Install(ctx, spec); err != nil {
		return err
	}

	logFile := filepath.Join(dir, "isp-probe.log")
	if cfg.Log.File != "" {
		logFile = cfg.Log.File
	}

	if *noStart {
		fmt.Println("\n✓ 服务已安装（未启动）")
		printSpec(spec, logFile, 0)
		fmt.Println("\n  启动: isp-probe service start")
		return nil
	}
	if err := mgr.Start(ctx); err != nil {
		return err
	}

	// 装完就验：轮询面板直到它自报家门，确认服务真的活着而不只是「已登记」。
	if id, ok := waitAlive(ctx, cfg.Web.Listen, 20*time.Second); ok {
		fmt.Println("\n✓ 服务已安装并启动")
		printSpec(spec, logFile, id.PID)
		fmt.Printf("\n  停止: isp-probe service stop\n  卸载: isp-probe service uninstall\n")
		if runtime.GOOS == "windows" && cfg.Notify.Desktop {
			fmt.Println("\n注意: Windows 服务运行在 session 0，与用户桌面隔离，桌面通知无法送达。")
			fmt.Println("  建议把 config.yaml 的 notify.desktop 改成 false，改用面板查看告警。")
		}
		return nil
	}

	fmt.Fprintf(os.Stderr, "\n✗ 服务已安装，但 %s 内没能响应面板请求\n", 20*time.Second)
	printLogTail(logFile, 20)
	return fmt.Errorf("排查: isp-probe service logs -n 100")
}

func serviceUninstall(ctx context.Context, mgr service.Manager) error {
	if err := mgr.Uninstall(ctx); err != nil {
		return err
	}
	fmt.Println("✓ 服务已停止并卸载")
	return nil
}

func serviceStart(ctx context.Context, mgr service.Manager) error {
	if err := mgr.Start(ctx); err != nil {
		return err
	}
	st, _ := mgr.Status(ctx)
	fmt.Printf("✓ 服务已启动（%s，PID %d）\n", st.State, st.PID)
	return nil
}

func serviceStop(ctx context.Context, mgr service.Manager) error {
	if err := mgr.Stop(ctx); err != nil {
		return err
	}
	fmt.Println("✓ 服务已停止（plist / 服务定义保留，下次登录仍会自启）")
	return nil
}

func serviceStatus(ctx context.Context, mgr service.Manager) error {
	st, err := mgr.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Installed {
		fmt.Println("服务未安装。用 isp-probe service install 安装。")
		return nil
	}

	fmt.Printf("状态    %s\n", st.State)
	if st.PID > 0 {
		fmt.Printf("PID     %d\n", st.PID)
	}
	if st.ExePath != "" {
		fmt.Printf("程序    %s\n", st.ExePath)
		// 装的是别处的二进制时，用户在这个目录里改代码重编译是不生效的。
		if cur := apppath.Discover().Exe; cur != "" && cur != st.ExePath {
			fmt.Printf("        （与当前运行的 %s 不是同一个）\n", cur)
		}
	}
	if st.CfgPath != "" {
		fmt.Printf("配置    %s\n", st.CfgPath)
	}

	// 平台的「已启动」只说明进程被拉起来了，不代表面板真的在服务。
	// 探一下才知道它是不是卡在初始化里。
	if st.CfgPath != "" {
		if cfg, err := config.Load(st.CfgPath); err == nil {
			if id, ok := waitAlive(ctx, cfg.Web.Listen, 2*time.Second); ok {
				fmt.Printf("面板    %s  (响应正常，PID %d)\n", displayURL(cfg.Web.Listen), id.PID)
			} else if st.Running {
				fmt.Printf("面板    %s  (进程在跑，但面板无响应 —— 看日志)\n", displayURL(cfg.Web.Listen))
			}
		}
	}
	return nil
}

func serviceLogs(ctx context.Context, mgr service.Manager, args []string) error {
	fs := flag.NewFlagSet("service logs", flag.ExitOnError)
	n := fs.Int("n", 50, "打印最后多少行")
	_ = fs.Parse(args)

	path, err := serviceLogPath(ctx, mgr)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n\n", path)
	printLogTail(path, *n)
	return nil
}

// serviceLogPath 从服务已登记的配置反推日志文件位置，
// 让用户不必自己记住装到哪儿了。
func serviceLogPath(ctx context.Context, mgr service.Manager) (string, error) {
	st, err := mgr.Status(ctx)
	if err == nil && st.CfgPath != "" {
		if cfg, err := config.Load(st.CfgPath); err == nil && cfg.Log.File != "" {
			return cfg.Log.File, nil
		}
		return filepath.Join(filepath.Dir(st.CfgPath), "logs", "isp-probe.log"), nil
	}

	// 服务没装或查不到时退回本地约定位置。
	paths := apppath.Discover()
	cfgFile, ferr := paths.FindConfig("")
	if ferr != nil {
		return "", fmt.Errorf("服务未安装，也找不到本地配置来推断日志位置")
	}
	return filepath.Join(filepath.Dir(cfgFile), "logs", "isp-probe.log"), nil
}

func printSpec(s service.Spec, logFile string, pid int) {
	fmt.Printf("\n  程序    %s\n", s.Exe)
	fmt.Printf("  配置    %s\n", s.Cfg)
	if pid > 0 {
		fmt.Printf("  PID     %d\n", pid)
	}
	fmt.Printf("  面板    %s\n", displayURL(s.Listen))
	fmt.Printf("  日志    %s\n", logFile)
}

// printLogTail 打印日志文件的最后 n 行。
//
// 安装校验失败时自动调用 —— 用户不必再去猜日志放在哪儿，
// 那正是最需要看到日志的时刻。
func printLogTail(path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "（读不到日志 %s: %v）\n", path, err)
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "（读取日志失败: %v）\n", err)
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		fmt.Println("（日志为空）")
		return
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		fmt.Println("  " + l)
	}
}

// waitAlive 轮询面板，直到它自报家门或超时。
func waitAlive(ctx context.Context, listen string, timeout time.Duration) (identity, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if id, ok := probeExisting(listen); ok {
			return id, true
		}
		if time.Now().After(deadline) {
			return identity{}, false
		}
		select {
		case <-ctx.Done():
			return identity{}, false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// warnSSH 拦截 SSH 会话下的安装。
//
// 从 SSH 里 bootstrap 的作业可能落到 user/$UID 而非 gui/$UID 域，
// 结果是桌面通知静默失效 —— 装完看起来一切正常，告警却永远收不到。
func warnSSH(force bool) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if os.Getenv("SSH_TTY") == "" && os.Getenv("SSH_CONNECTION") == "" {
		return nil
	}
	if force {
		fmt.Println("警告: 正在 SSH 会话中安装，桌面通知可能失效（已用 --force 忽略）")
		return nil
	}
	return fmt.Errorf("检测到 SSH 会话。从这里安装的 LaunchAgent 可能落到错误的域，\n" +
		"  导致桌面通知无法送达。请在本机图形会话的终端里安装，或加 --force 继续。")
}
