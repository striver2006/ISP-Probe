package main

// 一次性命令：跑完即退出，不常驻、不加单实例锁 ——
// 后台服务在跑的时候，用户仍然应该能随手 `isp-probe probe` 看一眼。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"isp-probe/internal/doctor"
	"isp-probe/internal/notify"
	"isp-probe/internal/probe"
	"isp-probe/internal/store"
)

// signalCtx 返回一个在 Ctrl-C / SIGTERM 时取消的 context。
func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// errSilent 表示「已经把结果打印给用户了，只差一个非零退出码」。
// doctor 的失败详情已逐条列出，再由 main 打印一行错误只会添乱。
var errSilent = errors.New("silent failure")

func runDoctor(o opts) error {
	e, err := loadEnv(o, true, false)
	if err != nil {
		return err
	}
	defer e.closer()

	ctx, cancel := signalCtx()
	defer cancel()

	fmt.Println("ISP探针 · 环境自检")
	fmt.Println("═══════════════════════════════════════════════════════")

	rep := doctor.Run(ctx, e.cfg, e.iface, e.binder)

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
		return errSilent
	}
	fmt.Println("\n自检通过，探测结果可信。")
	return nil
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

func runProbe(o opts) error {
	e, err := loadEnv(o, true, false)
	if err != nil {
		return err
	}
	defer e.closer()

	ctx, cancel := signalCtx()
	defer cancel()

	db, err := store.Open(e.cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer db.Close()

	m := probe.NewMonitor(e.cfg, e.binder, db, notify.Discard{}, e.log)
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
	return nil
}

// runSpeed 对当前出口所在的线路做一次测速。
//
// 只测「当前这条」：路由器按 MAC 绑定 WAN，一个 MAC 同时只能走一条线，
// 要测另一条得先去路由器改绑。完整的引导式两条线流程在 Web 面板里。
func runSpeed(o opts) error {
	e, err := loadEnv(o, true, false)
	if err != nil {
		return err
	}
	defer e.closer()

	ctx, cancel := signalCtx()
	defer cancel()

	db, err := store.Open(e.cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer db.Close()

	m := probe.NewMonitor(e.cfg, e.binder, db, notify.Discard{}, e.log)
	tester := probe.NewSpeedTester(e.binder, m.Resolver(), e.cfg.Speed)

	fmt.Println("正在识别当前出口线路…")
	eg, err := probe.LookupEgress(ctx, e.binder, m.Resolver(), e.cfg.Speed.EgressURLs)
	if err != nil {
		return fmt.Errorf("识别出口失败: %w", err)
	}

	linkName, linkID := "未知", ""
	desc := eg.ISP + " " + eg.Org + " " + eg.AS
	for i := range e.cfg.Links {
		if e.cfg.Links[i].MatchISP(desc) {
			linkName, linkID = e.cfg.Links[i].Name, e.cfg.Links[i].ID
			break
		}
	}
	fmt.Printf("出口 %s · 归属 %s · 判定线路: %s\n", eg.IP, eg.ISP, linkName)
	if e.iface.IsWireless {
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
			EgressIP: eg.IP, EgressISP: eg.ISP, Streams: e.cfg.Speed.Streams,
			Duration: e.cfg.Speed.Duration, Wireless: e.iface.IsWireless,
		})
	}
	return nil
}
