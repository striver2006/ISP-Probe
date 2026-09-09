package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"isp-probe/internal/applock"
	"isp-probe/internal/config"
	"isp-probe/internal/dnsx"
	"isp-probe/internal/httpx"
	"isp-probe/internal/notify"
	"isp-probe/internal/probe"
	"isp-probe/internal/service"
	"isp-probe/internal/store"
	"isp-probe/internal/web"
)

// runServe 是 serve 命令的入口，同时服务于前台和后台两种模式。
//
// 服务模式下的两点不同：日志落轮转文件（后台进程的 stderr 在 Windows
// 服务里等于丢弃），以及生命周期交给平台的托管循环 —— Windows 需要在
// SCM 的调度里上报状态，直接跑会被判定为启动失败。
func runServe(o opts) error {
	if service.IsManaged() {
		return service.RunManaged(func(ctx context.Context, ready func()) error {
			e, err := loadEnv(o, true, true)
			if err != nil {
				return err
			}
			defer e.closer()
			return serveLoop(ctx, e, ready)
		})
	}

	e, err := loadEnv(o, true, false)
	if err != nil {
		return err
	}
	defer e.closer()

	ctx, cancel := signalCtx()
	defer cancel()
	return serveLoop(ctx, e, func() {})
}

func serveLoop(ctx context.Context, e *env, ready func()) error {
	// 锁文件与数据库同目录：要保护的是这个 DB，两份配置指向同一个 DB 时
	// 锁才有意义。放在配置目录反而会漏掉那种情况。
	lockPath := filepath.Join(filepath.Dir(e.cfg.Store.Path), "isp-probe.lock")
	lock, err := applock.Acquire(lockPath)
	if err != nil {
		if errors.Is(err, applock.ErrLocked) {
			return conflictHint(e.cfg)
		}
		return err
	}
	defer lock.Release()

	// 锁只挡得住持锁的实例。端口还可能被别的东西占着 —— 旧版本的自己、
	// 手工起的进程、或某个无关程序。裸的 "bind: address already in use"
	// 对用户毫无帮助，这里提前探一下，好说清楚占用者是谁。
	if portInUse(e.cfg.Web.Listen) {
		return conflictHint(e.cfg)
	}

	db, err := store.Open(e.cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}

	n := buildNotifier(e)

	m := probe.NewMonitor(e.cfg, e.binder, db, n, e.log)
	e.log.Info("开始监测", "网卡", e.iface.Name, "线路数", len(e.cfg.Links),
		"间隔", e.cfg.Probe.Interval, "配置", e.cfgFile, "数据库", e.cfg.Store.Path)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go m.Run(ctx)

	srv := web.New(e.cfg, e.iface, e.binder, m, db, n, e.log)
	ready()
	runErr := srv.Run(ctx)

	// 面板自己挂了也要让监控停下来，等它把最后一轮采样写完，最后才关库。
	// 顺序不能颠倒，所以 db.Close 用显式调用而不是 defer。
	cancel()
	select {
	case <-m.Done():
	case <-time.After(8 * time.Second):
		e.log.Warn("监控未在 8 秒内退出，仍将关闭数据库")
	}
	// 队列里可能还压着刚才那条 down 通知 —— 恰恰是最该发出去的一条。
	// 预算递增不倒挂：落库 5s < Monitor.Done() 8s < 8+3s < SCM WaitHint 15s。
	n.Close(3 * time.Second)
	db.Close()

	if runErr != nil {
		return fmt.Errorf("面板启动失败: %w", runErr)
	}
	e.log.Info("已停止")
	return nil
}

// conflictHint 把「起不来」翻译成可操作的提示。
//
// 分三种情况说清楚，因为处理方式完全不同：占用者是我们自己（停掉它）、
// 是无关程序（改端口）、还是只有数据库被占而端口空着（另一个实例用了
// 不同的 web.listen）。光说「已有实例在跑」，用户还得自己去翻进程。
func conflictHint(cfg config.Config) error {
	if id, ok := probeExisting(cfg.Web.Listen); ok {
		mode := "前台"
		if id.Mode == "service" {
			mode = "后台服务"
		}
		return fmt.Errorf("面板端口 %s 上已经有一个 ISP探针 实例（PID %d，%s）。\n\n"+
			"  查看它:   isp-probe service status\n"+
			"  停掉它:   isp-probe service stop\n"+
			"  或改端口: 编辑 config.yaml 的 web.listen 与 store.path 后重试",
			cfg.Web.Listen, id.PID, mode)
	}
	if portInUse(cfg.Web.Listen) {
		return fmt.Errorf("面板端口 %s 已被占用，但占用者不是 ISP探针。\n\n"+
			"  查看占用者: lsof -nP -iTCP:%s -sTCP:LISTEN\n"+
			"  或改端口:   编辑 config.yaml 的 web.listen 后重试",
			cfg.Web.Listen, portOf(cfg.Web.Listen))
	}
	return fmt.Errorf("已经有一个 ISP探针 实例在使用同一个数据库 (%s)。\n\n"+
		"  查看它: isp-probe service status\n"+
		"  停掉它: isp-probe service stop", cfg.Store.Path)
}

func portOf(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil {
		return port
	}
	return listen
}

// portInUse 报告监听地址是否已被占用。
//
// 与 probeExisting 互补：那个只认得出「是我们自己」，这个连无关程序占了
// 端口也能提前发现，免得等到 ListenAndServe 才抛出裸的 bind 错误。
func portInUse(listen string) bool {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// displayURL 是给人看的面板地址。监听通配时 http://0.0.0.0:8686 无法点击，
// 换成环回；局域网地址由 serve 启动日志单独给出（那里才知道网卡 IP）。
func displayURL(listen string) string { return "http://" + dialTarget(listen) }

// dialTarget 把监听地址转成「从本机连得上」的地址。
//
// 0.0.0.0 / :: / 空 host 是「监听所有接口」的写法，不是可连接的目标：
// macOS 上连 0.0.0.0 碰巧会落到环回，Windows 上直接 WSAEADDRNOTAVAIL，
// 于是单实例保护静默失效，能重复起两个实例抢同一个数据库。
// listen 写成 ":8686" 更糟 —— 拼出来的 "http://:8686/..." 根本不是合法 URL。
func dialTarget(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return net.JoinHostPort("127.0.0.1", port)
	}
	return listen
}

// identity 是面板 /api/status 里的身份字段，用来确认「那头是我们自己」。
type identity struct {
	App  string `json:"app"`
	PID  int    `json:"pid"`
	Mode string `json:"mode"`
}

// probeExisting 探测目标地址上是否已有一个 ISP探针 在跑。
//
// 这里刻意用裸 net.Dialer 而不是 netbind.Binder：目标是环回地址，绑定物理
// 网卡去连 127.0.0.1 反而会失败。这不违反「所有出站 socket 必须经 Binder」
// —— 那条规则约束的是用于测量线路的流量，环回自检不产生任何出网包。
func probeExisting(listen string) (identity, bool) {
	var id identity
	c := &http.Client{
		Timeout: 800 * time.Millisecond,
		Transport: &http.Transport{
			Proxy:       nil, // 绝不能走系统代理，那会把请求送到别处
			DialContext: (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext,
		},
	}
	resp, err := c.Get("http://" + dialTarget(listen) + "/api/status")
	if err != nil {
		return id, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return id, false
	}
	if err := json.NewDecoder(resp.Body).Decode(&id); err != nil {
		return id, false
	}
	return id, id.App == web.AppID
}

// buildNotifier 按配置组装通知渠道。
//
// 开关在这里生效，而不是在状态机里再判一次 cfg.Notify.Desktop —— 那样
// 「关掉桌面通知、只用 IM」的配置会一条都发不出去。
func buildNotifier(e *env) *notify.Multi {
	var chans []notify.Channel
	if e.cfg.Notify.Desktop {
		chans = append(chans, notify.NewDesktop(30*time.Second))
	}
	if len(e.cfg.Notify.Webhooks) > 0 {
		// 单开一个 Resolver 而不是复用 Monitor 的：webhook 域名就那么一两个、
		// 查询也稀疏，独立缓存的代价可以忽略，换来不必调整 Monitor 的构造顺序。
		client := notifyClient(e)
		for _, w := range e.cfg.Notify.Webhooks {
			if !w.Enabled {
				continue
			}
			chans = append(chans, notify.NewWebhook(w, client, e.log))
		}
	}
	return notify.NewMulti(e.log, chans...)
}

// notifyClient 构造发送通知用的 HTTP 客户端：绑物理网卡 + DoH 解析。
//
// 通知也是出站流量，同样不能落到 clash TUN 上 —— 那会安静地成功，
// 于是「线路断了」的告警从一条根本没在测的链路上发了出去。
// 超时交给各渠道自己的 timeout 控制，这里不设。
func notifyClient(e *env) *http.Client {
	return httpx.NewClient(e.binder, dnsx.NewResolver(e.binder, nil), 0)
}
