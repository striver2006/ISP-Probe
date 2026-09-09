// Command isp-probe / ISP探针
//
// 定时检测双 WAN（电信 + 移动）各自的连通状态，并支持引导式分线测速。
// 全程绕过本机 clash verge 的 TUN 与 DNS 劫持，确保测到的是真实线路。
//
// 支持两种运行模式：前台按需运行（Ctrl-C 退出），或安装成随登录自启的
// 后台服务（见 service 子命令）。
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"isp-probe/internal/applog"
	"isp-probe/internal/apppath"
	"isp-probe/internal/config"
	"isp-probe/internal/iface"
	"isp-probe/internal/netbind"
)

const usage = `ISP探针 (ISP-Probe) — 双线宽带接入点监测

用法:
  isp-probe <命令> [选项]

按需运行（前台，Ctrl-C 退出）:
  doctor    环境自检：验证 clash TUN 绕过是否生效、分线通道是否可用
  probe     执行一次分线连通性探测并打印结果
  serve     常驻运行，定时探测并提供 Web 面板
  speed     对当前出口线路做一次上下行测速

后台服务:
  service install     安装为随登录自启的后台服务并启动
  service uninstall   停止并卸载
  service start       启动已安装的服务
  service stop        停止（不卸载，下次登录仍会自启）
  service status      查看安装与运行状态
  service logs [-n N] 查看服务日志尾部

通知:
  notify test         向已配置的 IM 渠道发一条测试消息

通用选项:
  -c <path>   配置文件路径（默认在当前目录、可执行文件目录依次查找 config.yaml）
  -v          输出调试日志
`

// opts 是所有命令共享的通用选项。
type opts struct {
	cfgPath string // -c 的原始值；留空表示用户没指定，走多目录查找
	verbose bool
	logDir  string // 仅 service install 用
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	// service 与 notify 是二级命令组，单独走一条解析路径。
	if cmd == "service" {
		os.Exit(runServiceCmd(os.Args[2:]))
	}
	if cmd == "notify" {
		os.Exit(runNotifyCmd(os.Args[2:]))
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var o opts
	bindGlobalFlags(fs, &o)
	_ = fs.Parse(os.Args[2:])

	var err error
	switch cmd {
	case "doctor":
		err = runDoctor(o)
	case "probe":
		err = runProbe(o)
	case "serve":
		err = runServe(o)
	case "speed":
		err = runSpeed(o)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		fmt.Print(usage)
		os.Exit(2)
	}

	if err != nil {
		// errSilent 的详情已经打印过了，只差一个非零退出码。
		if !errors.Is(err, errSilent) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func bindGlobalFlags(fs *flag.FlagSet, o *opts) {
	// 默认值留空而不是 "config.yaml"：要能区分「用户没指定」与「用户明确
	// 指定了 config.yaml」，只有前者才去多个目录里找。
	fs.StringVar(&o.cfgPath, "c", "", "配置文件路径")
	fs.BoolVar(&o.verbose, "v", false, "输出调试日志")
}

// env 是需要真实网络探测能力的命令的公共前置结果。
//
// 服务管理类命令（install 之外）不会构造它，因而不依赖配置加载与网卡探测 ——
// 那两步任一失败都会让 status / stop / uninstall 变得无法使用。
type env struct {
	paths   apppath.Paths
	cfgFile string
	cfg     config.Config
	iface   iface.PhysicalIface
	binder  *netbind.Binder
	log     *slog.Logger
	closer  func()
}

// loadEnv 解析路径 → 加载配置 → 建日志 → （可选）探测网卡。
//
// needIface=false 用于只需要读配置的场景；toFile=true 让日志落到轮转文件
// 而不是 stderr（服务模式）。
func loadEnv(o opts, needIface, toFile bool) (*env, error) {
	paths := apppath.Discover()

	cfgFile, err := paths.FindConfig(o.cfgPath)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("配置错误: %w", err)
	}

	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log, closer, err := applog.New(applog.Options{
		Level:   level,
		File:    logTarget(cfg, paths, o, toFile),
		MaxSize: cfg.Log.MaxSizeMB,
		Keep:    cfg.Log.Keep,
	})
	if err != nil {
		return nil, err
	}

	e := &env{paths: paths, cfgFile: cfgFile, cfg: cfg, log: log, closer: closer}
	if needIface {
		if e.iface, e.binder, err = setupIface(cfg); err != nil {
			closer()
			return nil, err
		}
	}
	return e, nil
}

// logTarget 决定诊断日志的去向。返回空串表示写 stderr。
func logTarget(cfg config.Config, p apppath.Paths, o opts, serviceMode bool) string {
	if o.logDir != "" {
		return filepath.Join(o.logDir, "isp-probe.log")
	}
	if cfg.Log.File != "" {
		return cfg.Log.File // config.Anchor 已把它绝对化
	}
	if serviceMode {
		return filepath.Join(p.Base, "logs", "isp-probe.log")
	}
	return "" // 前台：保持 stderr，与服务化之前完全一致
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

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
