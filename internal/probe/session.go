package probe

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/store"
)

// 测速会话的阶段。
const (
	PhaseIdle      = "idle"      // 未开始
	PhaseDetecting = "detecting" // 正在识别当前出口走的是哪条线
	// PhaseConfirm 等待用户确认要测哪条线。用户此刻可以先去路由器改绑 MAC，
	// 程序会自动察觉并更新待测线路，不必重新发起测速。
	PhaseConfirm    = "confirm"
	PhaseTesting    = "testing"     // 正在测速
	PhaseAwaitSwitch = "await_switch" // 等待用户在路由器上改绑 MAC 到另一条线
	PhaseDone       = "done"
	PhaseError      = "error"
)

// SessionState 是测速会话对外暴露的状态快照。
type SessionState struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`

	CurrentLinkID   string `json:"current_link_id"`
	CurrentLinkName string `json:"current_link_name"`
	Egress          Egress `json:"egress"`

	Progress SpeedProgress `json:"progress"`

	// Pending 是尚未测过的线路，用于提示用户还需切换到哪条。
	Pending     []PendingLink       `json:"pending"`
	Results     []store.SpeedResult `json:"results"`
	SwitchHint  SwitchHint          `json:"switch_hint"`
	Error       string              `json:"error,omitempty"`
	StartedAt   time.Time           `json:"started_at"`
	FinishedAt  time.Time           `json:"finished_at,omitempty"`
}

type PendingLink struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	WANLabel string `json:"wan_label"`
}

// SwitchHint 是给用户的操作指引：去哪、改什么。
type SwitchHint struct {
	Active    bool   `json:"active"`
	RouterURL string `json:"router_url"`
	PageHint  string `json:"page_hint"`
	MAC       string `json:"mac"`
	TargetWAN string `json:"target_wan"`
	TargetLink string `json:"target_link"`
}

// SpeedSession 编排引导式分线测速。
//
// 为什么需要引导：路由器只支持按 MAC 绑定 WAN 口策略，一个 MAC 同时只能
// 绑一条线，所以无法并行测两条。流程是「测当前这条 → 提示用户去路由器改绑
// → 自动检测到切换完成 → 测另一条」。
//
// 判断"当前走哪条线"和"切换是否完成"都靠出口 IP 的运营商归属 ——
// 主机自己无从得知路由器把它分到了哪个 WAN，只能从外部看回来。
type SpeedSession struct {
	cfg    config.Config
	tester *SpeedTester
	db     *store.Store
	log    *slog.Logger
	// wireless 记录测速链路是否为无线，写进结果以便解读带宽上限。
	wireless bool

	mu     sync.RWMutex
	state  SessionState
	cancel context.CancelFunc
	// confirmCh 接收用户在确认阶段点下「开始测试」的信号。
	confirmCh chan struct{}

	onChange func()
}

func NewSpeedSession(cfg config.Config, t *SpeedTester, db *store.Store, wireless bool, log *slog.Logger, onChange func()) *SpeedSession {
	return &SpeedSession{
		cfg: cfg, tester: t, db: db, log: log, wireless: wireless,
		onChange: onChange,
		state:    SessionState{Phase: PhaseIdle},
	}
}

func (s *SpeedSession) State() SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *SpeedSession) Running() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Phase != PhaseIdle && s.state.Phase != PhaseDone && s.state.Phase != PhaseError
}

func (s *SpeedSession) update(fn func(*SessionState)) {
	s.mu.Lock()
	fn(&s.state)
	s.mu.Unlock()
	if s.onChange != nil {
		s.onChange()
	}
}

// Start 启动一轮完整的分线测速。已在运行时返回错误。
func (s *SpeedSession) Start(parent context.Context) error {
	s.mu.Lock()
	if s.state.Phase != PhaseIdle && s.state.Phase != PhaseDone && s.state.Phase != PhaseError {
		s.mu.Unlock()
		return fmt.Errorf("测速已在进行中")
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	// 每轮重建，避免上一轮残留的确认信号让新一轮直接跳过确认。
	s.confirmCh = make(chan struct{}, 1)
	s.state = SessionState{Phase: PhaseDetecting, StartedAt: time.Now(),
		Message: "正在识别当前出口走的是哪条线路…"}
	s.mu.Unlock()

	go s.run(ctx)
	return nil
}

// Stop 中止当前测速。
func (s *SpeedSession) Stop() {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

func (s *SpeedSession) run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		if s.state.Phase != PhaseError {
			s.state.Phase = PhaseDone
			s.state.FinishedAt = time.Now()
			if len(s.state.Results) == len(s.cfg.Links) {
				s.state.Message = "两条线路均已测完"
			} else {
				s.state.Message = "测速结束"
			}
		}
		s.state.SwitchHint.Active = false
		s.mu.Unlock()
		if s.onChange != nil {
			s.onChange()
		}
	}()

	tested := make(map[string]bool)

	for len(tested) < len(s.cfg.Links) {
		if ctx.Err() != nil {
			return
		}

		link, eg, err := s.detectLink(ctx)
		if err != nil {
			s.fail(fmt.Sprintf("识别当前出口失败: %v", err))
			return
		}

		if tested[link.ID] {
			// 这条已经测过了，等用户切到剩下的那条。
			if !s.awaitSwitch(ctx, tested) {
				return
			}
			continue
		}

		// 开测第一条之前先让用户确认：当前 MAC 绑的是哪条线由路由器决定，
		// 用户很可能想先测另一条。这里给出当前绑定状态和改绑入口，
		// 等用户点头再开始 —— 而不是默认拿当前这条就测。
		if len(tested) == 0 {
			l, e, ok := s.awaitConfirm(ctx, link, eg)
			if !ok {
				return
			}
			link, eg = l, e
		}

		s.update(func(st *SessionState) {
			st.Phase = PhaseTesting
			st.CurrentLinkID = link.ID
			st.CurrentLinkName = link.Name
			st.Egress = eg
			st.SwitchHint.Active = false
			st.Message = fmt.Sprintf("正在测试 %s 线路（出口 %s）…", link.Name, eg.Describe())
		})

		res := s.testOne(ctx, link, eg)
		if ctx.Err() != nil {
			return
		}
		tested[link.ID] = true

		s.update(func(st *SessionState) {
			st.Results = append(st.Results, res)
			st.Progress = SpeedProgress{}
		})

		if err := s.db.InsertSpeed(ctx, res); err != nil {
			s.log.Warn("写入测速结果失败", "err", err)
		}

		if len(tested) >= len(s.cfg.Links) {
			return
		}
		if !s.awaitSwitch(ctx, tested) {
			return
		}
	}
}

// Confirm 是用户在确认阶段点下「开始测试」的入口。
func (s *SpeedSession) Confirm() error {
	s.mu.RLock()
	phase := s.state.Phase
	ch := s.confirmCh
	s.mu.RUnlock()

	if phase != PhaseConfirm || ch == nil {
		return fmt.Errorf("当前不处于等待确认阶段")
	}
	select {
	case ch <- struct{}{}:
	default: // 已经确认过，忽略重复点击
	}
	return nil
}

// awaitConfirm 展示当前绑定状态并等待用户确认。
//
// 等待期间每 5 秒复查一次出口归属：用户完全可能在这时才去路由器改绑，
// 程序察觉后直接更新待测线路，用户不需要退出重来。
//
// 返回最终确认时的线路。false 表示会话已取消。
func (s *SpeedSession) awaitConfirm(ctx context.Context, link *config.LinkConfig, eg Egress) (*config.LinkConfig, Egress, bool) {
	curLink, curEg := link, eg
	s.showConfirm(curLink, curEg)

	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, Egress{}, false

		case <-s.confirmCh:
			return curLink, curEg, true

		case <-poll.C:
			l, e, err := s.detectLink(ctx)
			if err != nil {
				continue // 改绑瞬间可能短暂不通，继续等
			}
			if l.ID != curLink.ID {
				curLink, curEg = l, e
				s.showConfirm(curLink, curEg)
			}
		}
	}
}

// showConfirm 更新确认阶段的状态：当前会测哪条线，以及想换一条该怎么做。
func (s *SpeedSession) showConfirm(link *config.LinkConfig, eg Egress) {
	var others []PendingLink
	for i := range s.cfg.Links {
		if s.cfg.Links[i].ID == link.ID {
			continue
		}
		others = append(others, PendingLink{
			ID: s.cfg.Links[i].ID, Name: s.cfg.Links[i].Name,
			WANLabel: s.cfg.Links[i].WANLabel,
		})
	}

	hint := SwitchHint{
		Active:    true,
		RouterURL: s.cfg.Router.AdminURL,
		PageHint:  s.cfg.Router.RulePageHint,
		MAC:       s.tester.binder.MAC,
	}
	if len(others) > 0 {
		hint.TargetWAN = others[0].WANLabel
		hint.TargetLink = others[0].Name
	}

	s.update(func(st *SessionState) {
		st.Phase = PhaseConfirm
		st.CurrentLinkID = link.ID
		st.CurrentLinkName = link.Name
		st.Egress = eg
		st.Pending = others
		st.SwitchHint = hint
		st.Message = fmt.Sprintf("当前本机走的是 %s 线路（出口 %s）", link.Name, eg.Describe())
	})
}

// detectLink 通过出口 IP 的运营商归属判断当前走的是哪条线。
func (s *SpeedSession) detectLink(ctx context.Context) (*config.LinkConfig, Egress, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	eg, err := LookupEgress(cctx, s.tester.binder, s.tester.resolver, s.cfg.Speed.EgressURLs)
	if err != nil {
		return nil, Egress{}, err
	}

	desc := eg.ISP + " " + eg.Org + " " + eg.AS
	for i := range s.cfg.Links {
		if s.cfg.Links[i].MatchISP(desc) {
			return &s.cfg.Links[i], eg, nil
		}
	}
	return nil, eg, fmt.Errorf("出口 %s 的归属 %q 未匹配任何已配置线路，"+
		"请检查配置中的 expect_isp", eg.IP, eg.ISP)
}

// testOne 对一条线路做完整的上下行测速。
func (s *SpeedSession) testOne(ctx context.Context, link *config.LinkConfig, eg Egress) store.SpeedResult {
	res := store.SpeedResult{
		TS: time.Now(), LinkID: link.ID,
		EgressIP: eg.IP, EgressISP: eg.ISP,
		Streams: s.cfg.Speed.Streams, Duration: s.cfg.Speed.Duration,
		Wireless: s.wireless,
	}

	onProgress := func(p SpeedProgress) {
		s.update(func(st *SessionState) { st.Progress = p })
	}

	down, _, err := s.tester.Download(ctx, onProgress)
	if err != nil {
		s.log.Warn("下行测速失败", "link", link.ID, "err", err)
		res.Note = "下行: " + err.Error()
	}
	res.DownMbps = down

	if ctx.Err() != nil {
		return res
	}

	s.update(func(st *SessionState) {
		st.Message = fmt.Sprintf("%s 下行 %.1f Mbps，正在测上行…", link.Name, down)
	})

	up, _, err := s.tester.Upload(ctx, onProgress)
	if err != nil {
		s.log.Warn("上行测速失败", "link", link.ID, "err", err)
		if res.Note != "" {
			res.Note += "; "
		}
		res.Note += "上行: " + err.Error()
	}
	res.UpMbps = up

	return res
}

// awaitSwitch 提示用户去路由器改绑 MAC，并轮询出口归属直到检测到切换完成。
//
// 返回 false 表示上下文已取消。
func (s *SpeedSession) awaitSwitch(ctx context.Context, tested map[string]bool) bool {
	var next *config.LinkConfig
	var pending []PendingLink
	for i := range s.cfg.Links {
		if !tested[s.cfg.Links[i].ID] {
			if next == nil {
				next = &s.cfg.Links[i]
			}
			pending = append(pending, PendingLink{
				ID: s.cfg.Links[i].ID, Name: s.cfg.Links[i].Name,
				WANLabel: s.cfg.Links[i].WANLabel,
			})
		}
	}
	if next == nil {
		return true
	}

	s.update(func(st *SessionState) {
		st.Phase = PhaseAwaitSwitch
		st.Pending = pending
		st.Progress = SpeedProgress{}
		st.Message = fmt.Sprintf("请到路由器把本机 MAC 改绑到「%s」，改好后会自动开始测 %s 线路",
			next.WANLabel, next.Name)
		st.SwitchHint = SwitchHint{
			Active:     true,
			RouterURL:  s.cfg.Router.AdminURL,
			PageHint:   s.cfg.Router.RulePageHint,
			MAC:        s.tester.binder.MAC,
			TargetWAN:  next.WANLabel,
			TargetLink: next.Name,
		}
	})

	// 每 5 秒查一次出口归属，检测到已切到未测过的线路即返回。
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			link, _, err := s.detectLink(ctx)
			if err != nil {
				continue // 切换瞬间可能短暂不通，继续等
			}
			if !tested[link.ID] {
				return true
			}
		}
	}
}

func (s *SpeedSession) fail(msg string) {
	s.update(func(st *SessionState) {
		st.Phase = PhaseError
		st.Error = msg
		st.Message = msg
		st.FinishedAt = time.Now()
	})
}
