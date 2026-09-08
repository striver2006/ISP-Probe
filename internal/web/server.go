// Package web 提供本地 Web 面板：实时状态、历史曲线、引导式测速。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/doctor"
	"isp-probe/internal/iface"
	"isp-probe/internal/netbind"
	"isp-probe/internal/probe"
	"isp-probe/internal/store"
)

//go:embed ui
var uiFS embed.FS

type Server struct {
	cfg     config.Config
	iface   iface.PhysicalIface
	binder  *netbind.Binder
	monitor *probe.Monitor
	session *probe.SpeedSession
	db      *store.Store
	log     *slog.Logger

	// 自检报告较慢（要发多次网络请求），缓存起来按需刷新。
	docMu   sync.RWMutex
	docRep  *doctor.Report
	docTime time.Time

	subMu sync.Mutex
	subs  map[chan struct{}]struct{}
}

func New(cfg config.Config, p iface.PhysicalIface, b *netbind.Binder,
	m *probe.Monitor, db *store.Store, log *slog.Logger) *Server {

	s := &Server{
		cfg: cfg, iface: p, binder: b, monitor: m, db: db, log: log,
		subs: make(map[chan struct{}]struct{}),
	}
	tester := probe.NewSpeedTester(b, m.Resolver(), cfg.Speed)
	s.session = probe.NewSpeedSession(cfg, tester, db, p.IsWireless, log, s.broadcast)
	return s
}

// Session 暴露测速会话，供 CLI 直接驱动。
func (s *Server) Session() *probe.SpeedSession { return s.session }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/doctor", s.handleDoctor)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/speeds", s.handleSpeeds)
	mux.HandleFunc("GET /api/speed/state", s.handleSpeedState)
	mux.HandleFunc("POST /api/speed/start", s.handleSpeedStart)
	mux.HandleFunc("POST /api/speed/confirm", s.handleSpeedConfirm)
	mux.HandleFunc("POST /api/speed/stop", s.handleSpeedStop)
	mux.HandleFunc("GET /api/stream", s.handleStream)

	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		s.log.Error("加载内嵌前端失败", "err", err)
	} else {
		mux.Handle("GET /", http.FileServerFS(sub))
	}
	return mux
}

// Run 启动 HTTP 服务并在 ctx 取消时优雅关闭。
func (s *Server) Run(ctx context.Context) error {
	// 把监控的变更事件转发给 SSE 订阅者。
	go func() {
		ch, stop := s.monitor.Subscribe()
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				s.broadcast()
			}
		}
	}()

	srv := &http.Server{
		Addr:              s.cfg.Web.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	s.log.Info("面板已启动", "地址", "http://"+s.cfg.Web.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// ---- SSE ----

func (s *Server) broadcast() {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan struct{}, 1)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}()

	// 立即推一次，避免前端等到下一次变更才有数据。
	s.writeEvent(w, flusher)

	// 心跳兼作保活，防止中间层掐断空闲连接。
	beat := time.NewTicker(20 * time.Second)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			s.writeEvent(w, flusher)
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) writeEvent(w http.ResponseWriter, f http.Flusher) {
	data, err := json.Marshal(s.statusPayload())
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}

// ---- API ----

type statusResponse struct {
	Links          []probe.LinkStatus  `json:"links"`
	CombinedRTTms  float64             `json:"combined_rtt_ms"`
	Speed          probe.SessionState  `json:"speed"`
	Iface          ifaceInfo           `json:"iface"`
	Router         routerInfo          `json:"router"`
	ProbeInterval  float64             `json:"probe_interval_sec"`
	Now            time.Time           `json:"now"`
}

type ifaceInfo struct {
	Name       string `json:"name"`
	IP         string `json:"ip"`
	MAC        string `json:"mac"`
	Gateway    string `json:"gateway"`
	IsWireless bool   `json:"is_wireless"`
	Hardware   string `json:"hardware"`
}

type routerInfo struct {
	AdminURL string `json:"admin_url"`
	PageHint string `json:"page_hint"`
}

func (s *Server) statusPayload() statusResponse {
	adminURL := s.cfg.Router.AdminURL
	if adminURL == "" && s.iface.Gateway.IsValid() {
		adminURL = "http://" + s.iface.Gateway.String()
	}
	return statusResponse{
		Links:         s.monitor.Status(),
		CombinedRTTms: float64(s.monitor.CombinedRTT().Microseconds()) / 1000,
		Speed:         s.session.State(),
		Iface: ifaceInfo{
			Name: s.iface.Name, IP: s.iface.IPv4.String(), MAC: s.iface.MAC,
			Gateway: s.iface.Gateway.String(), IsWireless: s.iface.IsWireless,
			Hardware: s.iface.HardwareDesc,
		},
		Router:        routerInfo{AdminURL: adminURL, PageHint: s.cfg.Router.RulePageHint},
		ProbeInterval: s.cfg.Probe.Interval.Seconds(),
		Now:           time.Now(),
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.statusPayload())
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"

	s.docMu.RLock()
	rep, at := s.docRep, s.docTime
	s.docMu.RUnlock()

	// 自检要发不少网络请求，默认复用 5 分钟内的结果。
	if rep == nil || refresh || time.Since(at) > 5*time.Minute {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		rep = doctor.Run(ctx, s.cfg, s.iface, s.binder)
		s.docMu.Lock()
		s.docRep, s.docTime = rep, time.Now()
		s.docMu.Unlock()
	}
	writeJSON(w, rep)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	link := r.URL.Query().Get("link")
	if link == "" {
		http.Error(w, "缺少 link 参数", http.StatusBadRequest)
		return
	}
	since := 6 * time.Hour
	if v := r.URL.Query().Get("hours"); v != "" {
		if h, err := time.ParseDuration(v + "h"); err == nil && h > 0 {
			since = h
		}
	}
	samples, err := s.db.RecentSamples(r.Context(), link, since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type point struct {
		TS    int64   `json:"ts"`
		OK    bool    `json:"ok"`
		RTT   float64 `json:"rtt_ms"`
		Detail string `json:"detail,omitempty"`
	}
	out := make([]point, 0, len(samples))
	for _, x := range samples {
		p := point{TS: x.TS.UnixMilli(), OK: x.OK, Detail: x.Detail}
		if x.DNSRTT > 0 {
			p.RTT = float64(x.DNSRTT.Microseconds()) / 1000
		} else if x.AnchorRTT > 0 {
			p.RTT = float64(x.AnchorRTT.Microseconds()) / 1000
		}
		out = append(out, p)
	}
	writeJSON(w, out)
}

// handleStats 返回各线路在指定时间窗口内的质量统计。
//
// 单独成一个接口而不并进 /api/status：它要扫描时间窗口内的全部采样，
// 而 status 会被 SSE 高频推送，两者的调用频率不该绑在一起。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	hours := 24.0
	if v := r.URL.Query().Get("hours"); v != "" {
		if d, err := time.ParseDuration(v + "h"); err == nil && d > 0 {
			hours = d.Hours()
		}
	}
	window := time.Duration(hours * float64(time.Hour))

	out := make(map[string]store.LinkStats, len(s.cfg.Links))
	for i := range s.cfg.Links {
		id := s.cfg.Links[i].ID
		st, err := s.db.Stats(r.Context(), id, window)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out[id] = st
	}
	writeJSON(w, out)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := s.db.RecentEvents(r.Context(), 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, evs)
}

func (s *Server) handleSpeeds(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.RecentSpeeds(r.Context(), 30)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, res)
}

func (s *Server) handleSpeedState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.session.State())
}

func (s *Server) handleSpeedStart(w http.ResponseWriter, r *http.Request) {
	// 用后台 context：测速要跨越多次用户操作（去路由器改绑），
	// 远长于单个 HTTP 请求的生命周期。
	if err := s.session.Start(context.Background()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]string{"status": "started"})
}

// handleSpeedConfirm 是用户在确认阶段点下「开始测试当前线路」的入口。
func (s *Server) handleSpeedConfirm(w http.ResponseWriter, r *http.Request) {
	if err := s.session.Confirm(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]string{"status": "confirmed"})
}

func (s *Server) handleSpeedStop(w http.ResponseWriter, r *http.Request) {
	s.session.Stop()
	writeJSON(w, map[string]string{"status": "stopped"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
