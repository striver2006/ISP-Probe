package probe

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/store"
)

// recorder 记下收到的每条通知，用来断言「什么时候该响、什么时候不该响」。
type recorder struct {
	mu  sync.Mutex
	evs []store.Event
}

func (r *recorder) Notify(_ context.Context, ev store.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
	return nil
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.evs))
	for i, e := range r.evs {
		out[i] = e.Kind
	}
	return out
}

// 递增而非固定间隔：刚断时提醒密（人可能就在电脑前），断久了变稀。
func TestRepeatDelay(t *testing.T) {
	cases := []struct {
		sent int
		want time.Duration
	}{
		{1, 5 * time.Minute},
		{2, 15 * time.Minute},
		{3, 30 * time.Minute},
		{4, 60 * time.Minute},
		{99, 60 * time.Minute}, // 之后恒定，不会无限拉长到收不到
	}
	for _, c := range cases {
		if got := repeatDelay(c.sent); got != c.want {
			t.Errorf("repeatDelay(%d) = %v，期望 %v", c.sent, got, c.want)
		}
	}
}

// newTestMonitor 造一个只走状态机、不碰网络的 Monitor，配一个临时数据库
// —— 有了它才能断言「重复提醒不写 events 表」这条不变量。
func newTestMonitor(t *testing.T, repeat bool) (*Monitor, *recorder, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Config{
		Links:  []config.LinkConfig{{ID: "telecom", Name: "电信"}},
		Probe:  config.ProbeConfig{FailThreshold: 1},
		Notify: config.NotifyConfig{RepeatAlert: repeat},
	}
	rec := &recorder{}
	m := &Monitor{
		cfg:      cfg,
		db:       db,
		notifier: rec,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		status:   map[string]*LinkStatus{},
	}
	m.status["telecom"] = &LinkStatus{ID: "telecom", Name: "电信", Healthy: true, Since: time.Now()}
	return m, rec, db
}

func failSample(ts time.Time) store.Sample {
	return store.Sample{TS: ts, LinkID: "telecom", OK: false, Detail: "光猫不可达"}
}

// 核心不变量：持续故障按 5/15/30/60 分钟提醒，中间的每轮探测不吭声。
//
// 时间判断全部基于 sample.TS 而不是 time.Now()，所以这里可以直接喂时间戳，
// 不必 sleep。
func TestRepeatAlertSchedule(t *testing.T) {
	m, rec, db := newTestMonitor(t, true)
	l := &m.cfg.Links[0]
	ctx := context.Background()
	t0 := time.Now()

	// 第一次失败即跨过阈值（FailThreshold=1），产生 down。
	m.updateStatus(ctx, l, failSample(t0))

	// 之后每分钟探测一次，跑满 65 分钟。
	for i := 1; i <= 65; i++ {
		m.updateStatus(ctx, l, failSample(t0.Add(time.Duration(i)*time.Minute)))
	}

	// 期望：down 之后在 +5、+20（5+15）、+50（20+30）分钟各提醒一次。
	// 第四次要等到 +110 分钟，超出本次窗口。
	want := []string{"down", "down_repeat", "down_repeat", "down_repeat"}
	got := rec.kinds()
	if len(got) != len(want) {
		t.Fatalf("通知次数不符\n期望 %v\n实得 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条通知类型不符\n期望 %v\n实得 %v", i+1, want, got)
		}
	}

	// events 表要保持「一次故障 = 一条 down + 一条 up」的忠实记录。
	// 重复提醒只是通知层行为，落进去会让面板的事件列表被刷屏。
	evs, err := db.RecentEvents(ctx, 100)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != "down" {
		t.Errorf("重复提醒不应写入 events 表，实得 %d 条: %+v", len(evs), evs)
	}
}

// 关掉开关就退化成「状态跃迁各发一次」。
func TestRepeatAlertDisabled(t *testing.T) {
	m, rec, _ := newTestMonitor(t, false)
	l := &m.cfg.Links[0]
	ctx := context.Background()
	t0 := time.Now()

	m.updateStatus(ctx, l, failSample(t0))
	for i := 1; i <= 65; i++ {
		m.updateStatus(ctx, l, failSample(t0.Add(time.Duration(i)*time.Minute)))
	}

	if got := rec.kinds(); len(got) != 1 || got[0] != "down" {
		t.Errorf("关闭重复提醒后应只有一条 down，实得 %v", got)
	}
}

// 恢复后计数必须清零，否则下一次故障的提醒节奏会直接跳到 60 分钟。
func TestRecoveryResetsAlertCount(t *testing.T) {
	m, _, _ := newTestMonitor(t, true)
	l := &m.cfg.Links[0]
	ctx := context.Background()
	t0 := time.Now()

	m.updateStatus(ctx, l, failSample(t0))
	for i := 1; i <= 60; i++ {
		m.updateStatus(ctx, l, failSample(t0.Add(time.Duration(i)*time.Minute)))
	}

	st := m.status["telecom"]
	if st.alertCount < 2 {
		t.Fatalf("前置条件不成立：应已发出多次提醒，实得 %d", st.alertCount)
	}

	// 走真实的恢复路径。
	m.updateStatus(ctx, l, store.Sample{
		TS: t0.Add(61 * time.Minute), LinkID: "telecom", OK: true,
	})

	if st.alertCount != 0 || !st.lastAlert.IsZero() {
		t.Errorf("恢复后重复提醒的记账应清零，实得 count=%d lastAlert=%v", st.alertCount, st.lastAlert)
	}
	if !st.Healthy {
		t.Error("成功采样后应回到健康状态")
	}
}
