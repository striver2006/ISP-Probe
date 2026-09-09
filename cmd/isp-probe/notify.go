package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/notify"
	"isp-probe/internal/store"
)

const notifyUsage = `isp-probe notify <子命令> [选项]

子命令:
  test    向所有已启用的通知渠道各发一条测试消息

选项:
  -c <path>   配置文件路径
  --name <n>  只测试指定名字的渠道
`

// runNotifyCmd 处理 notify 子命令组，返回进程退出码。
func runNotifyCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, notifyUsage)
		return 2
	}
	switch args[0] {
	case "test":
		return runNotifyTest(args[1:])
	case "-h", "--help", "help":
		fmt.Print(notifyUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n%s", args[0], notifyUsage)
		return 2
	}
}

// runNotifyTest 同步发一条测试消息，逐个报告成败。
//
// 需要网卡（needIface=true）：发送要绑物理网卡，否则会落到 clash TUN 上。
func runNotifyTest(args []string) int {
	fs := flag.NewFlagSet("notify test", flag.ExitOnError)
	var o opts
	bindGlobalFlags(fs, &o)
	name := fs.String("name", "", "只测试指定名字的渠道")
	_ = fs.Parse(args)

	e, err := loadEnv(o, true, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	hooks := e.cfg.Notify.Webhooks
	if len(hooks) == 0 {
		fmt.Printf("未配置任何 IM 通知渠道。\n在 %s 中添加 notify.webhooks 后重试（密钥放这里，该文件不进 git）。\n",
			filepath.Join(filepath.Dir(e.cfgFile), config.LocalName))
		return 0
	}

	ev := store.Event{
		TS:      time.Now(),
		LinkID:  "_test",
		Kind:    "down",
		Message: "这是一条来自 ISP探针 的测试消息，收到说明通知链路正常。",
	}

	ctx, stop := signalCtx()
	defer stop()

	client := notifyClient(e)
	failed := 0
	matched := 0
	for _, w := range hooks {
		if *name != "" && w.Name != *name {
			continue
		}
		matched++
		if !w.Enabled {
			fmt.Printf("-  %s（%s）已禁用，跳过\n", w.Name, w.Kind)
			continue
		}
		ch := notify.NewWebhook(w, client, e.log)
		err := ch.Send(ctx, ev)
		ch.Close(time.Second)
		if err != nil {
			fmt.Printf("✗  %s（%s → %s）：%v\n", w.Name, w.Kind, notify.MaskURL(w.URL), err)
			failed++
			continue
		}
		fmt.Printf("✓  %s（%s → %s）\n", w.Name, w.Kind, notify.MaskURL(w.URL))
	}

	if matched == 0 {
		fmt.Fprintf(os.Stderr, "没有名为 %q 的渠道\n", *name)
		return 1
	}
	if failed > 0 {
		return 1
	}
	return 0
}
