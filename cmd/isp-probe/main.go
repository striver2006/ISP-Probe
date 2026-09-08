// Command isp-probe / ISP探针
//
// 定时检测双 WAN（电信 + 移动）各自的连通状态，并支持引导式分线测速。
// 全程绕过本机 clash verge 的 TUN 与 DNS 劫持，确保测到的是真实线路。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/doctor"
	"isp-probe/internal/iface"
	"isp-probe/internal/netbind"
	"isp-probe/internal/notify"
	"isp-probe/internal/probe"
	"isp-probe/internal/store"
	"isp-probe/internal/web"
)

const usage = `ISP探针 (ISP-Probe) — 双线宽带接入点监测

用法:
  isp-probe <命令> [选项]

命令:
  doctor    环境自检：验证 clash TUN 绕过是否生效、分线通道是否可用
  probe     执行一次分线连通性探测并打印结果
  serve     常驻运行，定时探测并提供 Web 面板
  speed     对当前出口线路做一次上下行测速

通用选项:
  -c <path>   配置文件路径 (默认 config.yaml)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("c", "config.yaml", "配置文件路径")
	verbose := fs.Bool("v", false, "输出调试日志")
	_ = fs.Parse(os.Args[2:])

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("配置错误: %v", err)
	}

	p, b, err := setupIface(cfg)
	if err != nil {
		fatal("%v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch cmd {
	case "doctor":
		runDoctor(ctx, cfg, p, b)
	case "probe":
		runProbe(ctx, cfg, b, log)
	case "serve":
		runServe(ctx, cfg, p, b, log)
	case "speed":
		runSpeed(ctx, cfg, p, b, log)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		fmt.Print(usage)
		os.Exit(2)
	}
}

// setupIface 确定要绑定的物理网卡。
func setupIface(cfg config.Config) (iface.PhysicalIface, *netbind.Binder, error) {
	var p iface.PhysicalIface
	var err error
	if cfg.Iface.Name != "" {
		p, err = iface.ByName(cfg.Iface.Name)
	} else {
		p, err = iface.Detect()
	}
	if err != nil {
		return p, nil, fmt.Errorf("识别物理网卡失败: %w", err)
	}
	b := &netbind.Binder{
		Name: p.Name, Index4: p.Index4, Index6: p.Index6,
		IPv4: p.IPv4, IPv6: p.IPv6, MAC: p.MAC,
		BindLocalAddr: cfg.Iface.BindLocalAddr,
	}
	return p, b, nil
}

func runDoctor(ctx context.Context, cfg config.Config, p iface.PhysicalIface, b *netbind.Binder) {
	fmt.Println("ISP探针 · 环境自检")
	fmt.Println("═══════════════════════════════════════════════════════")

	rep := doctor.Run(ctx, cfg, p, b)

	for _, c := range rep.Checks {
		fmt.Printf("\n%s %s\n", levelIcon(c.Level), c.Name)
		fmt.Printf("   %s\n", c.Detail)
		if c.Hint != "" {
			fmt.Printf("   ↳ %s\n", c.Hint)
		}
	}

	fmt.Println("\n═══════════════════════════════════════════════════════")
	fmt.Printf("绕过 TUN: %s   分线通道: %s   明文DNS: %s   IPv6: %s\n",
		yesNo(rep.BindWorks), yesNo(rep.ModemChannelOK),
		yesNo(rep.DNSPort53Usable), yesNo(rep.IPv6Usable))

	if rep.HasFailure() {
		fmt.Println("\n存在导致数据不可信的问题，请先处理上面标记为 ✗ 的项。")
		os.Exit(1)
	}
	fmt.Println("\n自检通过，探测结果可信。")
}

func runProbe(ctx context.Context, cfg config.Config, b *netbind.Binder, log *slog.Logger) {
	db, err := store.Open(cfg.Store.Path)
	if err != nil {
		fatal("打开数据库失败: %v", err)
	}
	defer db.Close()

	m := probe.NewMonitor(cfg, b, db, notify.Discard{}, log)
	m.RunOnce(ctx)

	fmt.Println("\n线路状态:")
	for _, s := range m.Status() {
		icon := "✓"
		if !s.Healthy {
			icon = "✗"
		}
		fmt.Printf("  %s %-6s DNS %.0fms  解析=%v  光猫=%s  上游递归=%s\n",
			icon, s.Name, s.DNSRTTms, s.Resolved,
			yesNo(s.ModemAlive), yesNo(s.ForcedOK))
		if s.Reason != "" {
			fmt.Printf("      原因: %s\n", s.Reason)
		}
	}
	if rtt := m.CombinedRTT(); rtt > 0 {
		fmt.Printf("\n合并出口锚点延迟: %.1fms\n", float64(rtt.Microseconds())/1000)
		fmt.Println("  (该指标无法归属到某条线：TCP 走哪条线由路由器负载均衡决定)")
	}
}

func runServe(ctx context.Context, cfg config.Config, p iface.PhysicalIface, b *netbind.Binder, log *slog.Logger) {
	db, err := store.Open(cfg.Store.Path)
	if err != nil {
		fatal("打开数据库失败: %v", err)
	}
	defer db.Close()

	var n probe.Notifier = notify.Discard{}
	if cfg.Notify.Desktop {
		n = notify.NewDesktop(30 * time.Second)
	}

	m := probe.NewMonitor(cfg, b, db, n, log)
	log.Info("开始监测", "网卡", p.Name, "线路数", len(cfg.Links), "间隔", cfg.Probe.Interval)

	go m.Run(ctx)

	srv := web.New(cfg, p, b, m, db, log)
	if err := srv.Run(ctx); err != nil {
		fatal("面板启动失败: %v", err)
	}
	log.Info("已停止")
}

// runSpeed 对当前出口所在的线路做一次测速。
//
// 只测「当前这条」：路由器按 MAC 绑定 WAN，一个 MAC 同时只能走一条线，
// 要测另一条得先去路由器改绑。完整的引导式两条线流程在 Web 面板里。
func runSpeed(ctx context.Context, cfg config.Config, p iface.PhysicalIface, b *netbind.Binder, log *slog.Logger) {
	db, err := store.Open(cfg.Store.Path)
	if err != nil {
		fatal("打开数据库失败: %v", err)
	}
	defer db.Close()

	m := probe.NewMonitor(cfg, b, db, notify.Discard{}, log)
	tester := probe.NewSpeedTester(b, m.Resolver(), cfg.Speed)

	fmt.Println("正在识别当前出口线路…")
	eg, err := probe.LookupEgress(ctx, b, m.Resolver(), cfg.Speed.EgressURLs)
	if err != nil {
		fatal("识别出口失败: %v", err)
	}

	linkName, linkID := "未知", ""
	desc := eg.ISP + " " + eg.Org + " " + eg.AS
	for i := range cfg.Links {
		if cfg.Links[i].MatchISP(desc) {
			linkName, linkID = cfg.Links[i].Name, cfg.Links[i].ID
			break
		}
	}
	fmt.Printf("出口 %s · 归属 %s · 判定线路: %s\n", eg.IP, eg.ISP, linkName)
	if p.IsWireless {
		fmt.Println("提示: 当前经 Wi-Fi 测速，吞吐上限受无线速率制约")
	}
	fmt.Println()

	show := func(pr probe.SpeedProgress) {
		tag := "下行"
		if pr.Phase == "upload" {
			tag = "上行"
		}
		warm := ""
		if pr.Warmup {
			warm = " (预热中)"
		}
		fmt.Printf("\r  %s %7.1f Mbps  %3.0f%%%s      ", tag, pr.Mbps, pr.Percent, warm)
	}

	down, downBytes, err := tester.Download(ctx, show)
	fmt.Println()
	if err != nil {
		fmt.Println("  下行测速失败:", err)
	} else {
		fmt.Printf("  下行: %.1f Mbps  (共 %.1f MB)\n", down, float64(downBytes)/1e6)
	}

	up, upBytes, err := tester.Upload(ctx, show)
	fmt.Println()
	if err != nil {
		fmt.Println("  上行测速失败:", err)
	} else {
		fmt.Printf("  上行: %.1f Mbps  (共 %.1f MB)\n", up, float64(upBytes)/1e6)
	}

	if linkID != "" {
		_ = db.InsertSpeed(ctx, store.SpeedResult{
			TS: time.Now(), LinkID: linkID, DownMbps: down, UpMbps: up,
			EgressIP: eg.IP, EgressISP: eg.ISP, Streams: cfg.Speed.Streams,
			Duration: cfg.Speed.Duration, Wireless: p.IsWireless,
		})
	}
}

func levelIcon(l doctor.Level) string {
	switch l {
	case doctor.LevelOK:
		return "✓"
	case doctor.LevelFail:
		return "✗"
	case doctor.LevelWarn:
		return "!"
	default:
		return "·"
	}
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
