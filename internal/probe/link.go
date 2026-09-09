// Package probe 实现各类探测：分线连通性、延迟、出口归属、带宽。
package probe

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/dnsx"
	"isp-probe/internal/netbind"
	"isp-probe/internal/store"
)

// CombinedLinkID 是「合并出口」这一伪线路的标识。
//
// 为什么需要它：TCP 连接建立后走哪条 WAN 由路由器的负载均衡决定，主机无法
// 指定，所以锚点 RTT 这类指标**不能归属到某一条线**，只能代表合并后的出口。
// 把它们诚实地记在这里，而不是随便挂到某条线上冒充分线数据。
const CombinedLinkID = "_combined"

// Notifier 是告警通知的抽象，便于测试替换。
//
// 传整个 store.Event 而非拼好的 (title, message)：IM 渠道要按 Kind 过滤、
// 要区分展示样式，只给两个字符串就什么都做不了。
type Notifier interface {
	Notify(ctx context.Context, ev store.Event) error
}

// LinkStatus 是一条线路的当前状态。
type LinkStatus struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Healthy  bool      `json:"healthy"`
	Since    time.Time `json:"since"`     // 当前状态的起始时刻
	LastOK   time.Time `json:"last_ok"`   // 最后一次成功
	LastSeen time.Time `json:"last_seen"` // 最后一次探测

	ConsecutiveFail int    `json:"consecutive_fail"`
	Reason          string `json:"reason"`

	DNSRTTms   float64  `json:"dns_rtt_ms"`
	ModemAlive bool     `json:"modem_alive"`
	Resolved   []string `json:"resolved"`
	// Distinct 表示本线的解析结果与其它线不同，是分线通道确实生效的证据。
	// 若两条线返回完全相同的节点，说明流量没有按预期分到不同 WAN。
	Distinct bool `json:"distinct"`
	// ForcedOK 表示最近一次强制递归查询成功，即该线上游真实可达
	// （而不只是命中了光猫的本地缓存）。
	ForcedOK bool `json:"forced_ok"`

	// 重复提醒的记账。不进 JSON —— 面板不需要，它看的是 Since。
	lastAlert  time.Time // 上次发出提醒的时刻（含首次 down）
	alertCount int       // 已发提醒次数，恢复时清零
}

// Monitor 负责周期性探测所有线路并维护状态机。
type Monitor struct {
	cfg      config.Config
	binder   *netbind.Binder
	modem    *dnsx.ModemProbe
	resolver *dnsx.Resolver
	db       *store.Store
	notifier Notifier
	log      *slog.Logger

	mu     sync.RWMutex
	status map[string]*LinkStatus
	round  int

	// 合并出口的最近一次锚点延迟
	combinedRTT time.Duration

	subMu sync.Mutex
	subs  map[chan struct{}]struct{}

	// done 在 Run 返回时关闭，让调用方能在关数据库之前等它收尾。
	done chan struct{}
}

func NewMonitor(cfg config.Config, b *netbind.Binder, db *store.Store, n Notifier, log *slog.Logger) *Monitor {
	m := &Monitor{
		cfg:      cfg,
		binder:   b,
		modem:    dnsx.NewModemProbe(b, cfg.Probe.MinGap, cfg.Probe.Timeout),
		resolver: dnsx.NewResolver(b, nil),
		db:       db,
		notifier: n,
		log:      log,
		status:   make(map[string]*LinkStatus),
		subs:     make(map[chan struct{}]struct{}),
		done:     make(chan struct{}),
	}
	for _, l := range cfg.Links {
		m.status[l.ID] = &LinkStatus{ID: l.ID, Name: l.Name, Healthy: true, Since: time.Now()}
	}
	return m
}

// Resolver 暴露内部解析器，供测速等模块复用其缓存。
func (m *Monitor) Resolver() *dnsx.Resolver { return m.resolver }

// Status 返回所有线路的状态快照。
func (m *Monitor) Status() []LinkStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]LinkStatus, 0, len(m.status))
	for _, l := range m.cfg.Links {
		if s, ok := m.status[l.ID]; ok {
			out = append(out, *s)
		}
	}
	return out
}

// CombinedRTT 返回合并出口的最近锚点延迟。
func (m *Monitor) CombinedRTT() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.combinedRTT
}

// Subscribe 注册一个变更通知通道，用于 Web 面板的 SSE 推送。
func (m *Monitor) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	m.subMu.Lock()
	m.subs[ch] = struct{}{}
	m.subMu.Unlock()
	return ch, func() {
		m.subMu.Lock()
		delete(m.subs, ch)
		close(ch)
		m.subMu.Unlock()
	}
}

func (m *Monitor) broadcast() {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default: // 订阅者尚未消费上一次通知，跳过即可
		}
	}
}

// Done 在 Run 返回后关闭。
//
// 调用方据此确保最后一轮采样已经落库，再去关闭数据库 —— 否则
// db.Close() 可能先于写入完成，本轮数据白采。
func (m *Monitor) Done() <-chan struct{} { return m.done }

// Run 启动周期探测，直到 ctx 取消。
func (m *Monitor) Run(ctx context.Context) {
	defer close(m.done)

	m.RunOnce(ctx)

	t := time.NewTicker(m.cfg.Probe.Interval)
	defer t.Stop()
	rollup := time.NewTicker(5 * time.Minute)
	defer rollup.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.RunOnce(ctx)
		case <-rollup.C:
			if err := m.db.Rollup(ctx, m.cfg.Store.RawRetain); err != nil {
				m.log.Warn("聚合历史数据失败", "err", err)
			}
		}
	}
}

// RunOnce 对所有线路各做一次探测。
func (m *Monitor) RunOnce(ctx context.Context) {
	m.mu.Lock()
	m.round++
	round := m.round
	m.mu.Unlock()

	// 合并出口的锚点延迟：与分线探测并发进行。
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.probeCombined(ctx)
	}()

	// 每条线并发探测。ModemProbe 内部按光猫地址各自限速，
	// 不同光猫之间互不影响，可以放心并发。
	samples := make([]store.Sample, len(m.cfg.Links))
	for i := range m.cfg.Links {
		l := &m.cfg.Links[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			samples[i] = m.checkLink(ctx, l, round)
		}()
	}
	wg.Wait()

	// 采样齐了才能做交叉验证 —— 它比较的是各线之间的差异。
	markDistinct(samples)

	// 落库阶段脱离 ctx 的取消：退出时 ctx 已经取消，若沿用它，ExecContext
	// 会立刻返回 context.Canceled，本轮辛苦采到的数据一条都写不进去。
	// 探测本身该被取消，写入不该。
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer wcancel()

	for i := range m.cfg.Links {
		if err := m.db.InsertSample(wctx, samples[i]); err != nil {
			m.log.Warn("写入采样失败", "link", m.cfg.Links[i].ID, "err", err)
		}
		m.updateStatus(wctx, &m.cfg.Links[i], samples[i])
	}
	m.broadcast()
}

// markDistinct 交叉验证各线路的解析结果是否互不相同。
//
// 两条线经各自光猫解析同一个域名，正常情况下会拿到各自运营商 CDN 的节点
// （本机实测：电信 180.101.x.x，移动 223.109.x.x）。如果它们返回完全相同
// 的结果，说明流量并没有按预期分到不同的 WAN 口 —— 分线通道已经失效，
// 此时「两条线都正常」的结论是不可信的。
//
// 这个检查不需要任何外部 IP 归属库，是免费的正确性保障。
func markDistinct(samples []store.Sample) {
	key := func(s store.Sample) string {
		r := append([]string(nil), s.Resolved...)
		sort.Strings(r)
		return strings.Join(r, ",")
	}

	// 统计每种解析结果出现了几次（只看成功且拿到了地址的采样）。
	count := make(map[string]int)
	for _, s := range samples {
		if s.OK && len(s.Resolved) > 0 {
			count[key(s)]++
		}
	}
	for i := range samples {
		s := &samples[i]
		if !s.OK || len(s.Resolved) == 0 {
			continue
		}
		// 只有当这份结果为本线独有时，才算分线有效。
		s.DistinctOK = count[key(*s)] == 1
		if !s.DistinctOK && s.Detail == "" {
			s.Detail = "解析结果与另一条线路完全相同，分线通道可能已失效"
		}
	}
}

// probeCombined 测量合并出口的锚点延迟。
func (m *Monitor) probeCombined(ctx context.Context) {
	rtt, err := AnchorRTT(ctx, m.binder, m.cfg.Probe.Anchors, m.cfg.Probe.Timeout)
	if err != nil {
		m.log.Debug("锚点延迟探测失败", "err", err)
		return
	}
	m.mu.Lock()
	m.combinedRTT = rtt
	m.mu.Unlock()

	_ = m.db.InsertSample(ctx, store.Sample{
		TS: time.Now(), LinkID: CombinedLinkID, OK: true, AnchorRTT: rtt,
	})
}

// checkLink 对单条线路做一次完整探测与状态判定。
//
// 故障判定分三级，目的是不把光猫限速误报成线路故障 —— 实测中快速连发
// DNS 查询会让光猫全部超时，而线路本身完好。
func (m *Monitor) checkLink(ctx context.Context, l *config.LinkConfig, round int) store.Sample {
	sample := store.Sample{TS: time.Now(), LinkID: l.ID}

	// 第一级：常规查询
	q := m.modem.Query(ctx, l.ModemAddr(), m.cfg.Probe.CacheDomain)

	if !q.OK() {
		// 第二级：光猫本身是否在线？这一步不经过 WAN，用于隔离故障域。
		alive, _ := m.modem.Alive(ctx, l.ModemAddr(), l.ModemPorts)
		sample.ModemAlive = alive

		if !alive {
			sample.Detail = "光猫不可达（本地链路故障或光猫掉电）"
		} else {
			// 第三级：光猫在线，降速重试排除限速。
			ok := false
			for i := 0; i < m.cfg.Probe.RetrySlow; i++ {
				select {
				case <-ctx.Done():
					return sample
				case <-time.After(time.Duration(i+1) * 2 * time.Second):
				}
				if r := m.modem.Query(ctx, l.ModemAddr(), m.cfg.Probe.CacheDomain); r.OK() {
					q = r
					ok = true
					break
				}
			}
			if !ok {
				sample.Detail = "线路故障：光猫在线但上游 DNS 无响应"
			}
		}
	} else {
		sample.ModemAlive = true
	}

	if q.OK() {
		sample.OK = true
		sample.DNSRTT = q.RTT
		for _, a := range q.Addrs {
			sample.Resolved = append(sample.Resolved, a.String())
		}
		// 每 5 轮做一次强制递归，验证上游真实可达而非命中光猫缓存。
		// 不每轮都做是为了避免触发光猫的速率限制。
		if round%5 == 1 {
			f := m.modem.QueryForced(ctx, l.ModemAddr(), m.cfg.Probe.ForceDomain)
			sample.ForcedOK = f.OK()
			if !f.OK() {
				sample.Detail = "强制递归查询失败：疑似上游 DNS 异常"
			}
		} else {
			sample.ForcedOK = true // 本轮未检查，不视为异常
		}
	}

	return sample
}

// updateStatus 推进状态机并在跨越阈值时告警。
func (m *Monitor) updateStatus(ctx context.Context, l *config.LinkConfig, s store.Sample) {
	m.mu.Lock()
	st := m.status[l.ID]
	if st == nil {
		st = &LinkStatus{ID: l.ID, Name: l.Name, Healthy: true, Since: time.Now()}
		m.status[l.ID] = st
	}

	st.LastSeen = s.TS
	st.ModemAlive = s.ModemAlive
	st.Resolved = s.Resolved
	st.ForcedOK = s.ForcedOK
	st.Distinct = s.DistinctOK
	st.DNSRTTms = float64(s.DNSRTT.Microseconds()) / 1000

	// event 是状态跃迁，要落库；repeat 是持续故障的重复提醒，只通知不落库。
	var event *store.Event
	var repeat *store.Event

	if s.OK {
		st.LastOK = s.TS
		st.ConsecutiveFail = 0
		if !st.Healthy {
			st.Healthy = true
			st.Since = s.TS
			st.Reason = "已恢复"
			st.lastAlert = time.Time{}
			st.alertCount = 0
			event = &store.Event{TS: s.TS, LinkID: l.ID, Kind: "up",
				Message: fmt.Sprintf("%s 线路已恢复", l.Name)}
		} else {
			st.Reason = ""
		}
	} else {
		st.ConsecutiveFail++
		st.Reason = s.Detail
		switch {
		case st.Healthy && st.ConsecutiveFail >= m.cfg.Probe.FailThreshold:
			st.Healthy = false
			st.Since = s.TS
			st.lastAlert = s.TS
			st.alertCount = 1
			event = &store.Event{TS: s.TS, LinkID: l.ID, Kind: "down",
				Message: fmt.Sprintf("%s 线路故障：%s", l.Name, s.Detail)}

		case !st.Healthy && m.cfg.Notify.RepeatAlert &&
			s.TS.Sub(st.lastAlert) >= repeatDelay(st.alertCount):
			// 故障还在持续。只发通知、不写 events —— 那张表要保持
			// 「一次故障 = 一条 down + 一条 up」，否则面板事件列表会被刷屏。
			st.lastAlert = s.TS
			st.alertCount++
			mins := int(s.TS.Sub(st.Since).Minutes())
			repeat = &store.Event{TS: s.TS, LinkID: l.ID, Kind: "down_repeat",
				Message: fmt.Sprintf("%s 线路仍未恢复（已持续 %d 分钟）：%s", l.Name, mins, s.Detail)}
		}
	}
	m.mu.Unlock()

	if event != nil {
		if err := m.db.InsertEvent(ctx, *event); err != nil {
			m.log.Warn("写入事件失败", "err", err)
		}
		m.log.Info("线路状态变更", "link", l.ID, "kind", event.Kind, "msg", event.Message)
	}

	// 通知开关下沉到构造 Notifier 时决定（见 serve.go）。这里再判一次
	// cfg.Notify.Desktop 会让「关掉桌面通知、只用 IM」的配置一条都发不出去。
	out := event
	if out == nil {
		out = repeat
	}
	if out == nil || m.notifier == nil {
		return
	}
	if err := m.notifier.Notify(ctx, *out); err != nil {
		m.log.Warn("发送通知失败", "err", err)
	}
}

// repeatDelay 是持续故障第 n 次提醒后、到下一次提醒的间隔。
//
// 递增而非固定：刚断的时候提醒密一些（你可能正好在电脑前，能马上处理），
// 断久了变稀（已经知道了，不必每半小时骚扰一次）。
func repeatDelay(sent int) time.Duration {
	switch sent {
	case 1:
		return 5 * time.Minute
	case 2:
		return 15 * time.Minute
	case 3:
		return 30 * time.Minute
	default:
		return 60 * time.Minute
	}
}
