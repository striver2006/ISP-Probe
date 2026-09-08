// Package store 负责探测数据的持久化。
//
// 用 modernc.org/sqlite（纯 Go 实现），使得 CGO_ENABLED=0 就能交叉编译到
// Windows 与 macOS 的各架构，代价是写入吞吐低于 CGO 版本 —— 对每分钟几条
// 采样的量级完全够用。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", path, err)
	}
	// 纯 Go 驱动下并发写会相互阻塞，限制连接数以避免 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Sample 是一次连通性采样。
type Sample struct {
	TS          time.Time
	LinkID      string
	OK          bool
	DNSRTT      time.Duration
	AnchorRTT   time.Duration
	ForcedOK    bool
	ModemAlive  bool
	DistinctOK  bool
	Resolved    []string
	Detail      string
}

func (s *Store) InsertSample(ctx context.Context, x Sample) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO samples_raw
		 (ts, link_id, ok, dns_rtt_ms, anchor_rtt_ms, forced_ok, modem_alive, distinct_ok, resolved, detail)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		x.TS.UnixMilli(), x.LinkID, b2i(x.OK),
		msOrNil(x.DNSRTT), msOrNil(x.AnchorRTT),
		b2i(x.ForcedOK), b2i(x.ModemAlive), b2i(x.DistinctOK),
		strings.Join(x.Resolved, ","), x.Detail)
	return err
}

// SpeedResult 是一次测速结果。
type SpeedResult struct {
	TS        time.Time
	LinkID    string
	DownMbps  float64
	UpMbps    float64
	EgressIP  string
	EgressISP string
	Streams   int
	Duration  time.Duration
	Wireless  bool
	Note      string
}

func (s *Store) InsertSpeed(ctx context.Context, x SpeedResult) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO speed_tests
		 (ts, link_id, down_mbps, up_mbps, egress_ip, egress_isp, streams, duration_ms, wireless, note)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		x.TS.UnixMilli(), x.LinkID, x.DownMbps, x.UpMbps, x.EgressIP, x.EgressISP,
		x.Streams, x.Duration.Milliseconds(), b2i(x.Wireless), x.Note)
	return err
}

// Event 是一次状态变更。
type Event struct {
	TS      time.Time
	LinkID  string
	Kind    string
	Message string
}

func (s *Store) InsertEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (ts, link_id, kind, message) VALUES (?,?,?,?)`,
		e.TS.UnixMilli(), e.LinkID, e.Kind, e.Message)
	return err
}

// RecentSamples 返回某条线路最近 since 时间内的采样，按时间升序。
func (s *Store) RecentSamples(ctx context.Context, linkID string, since time.Duration) ([]Sample, error) {
	from := time.Now().Add(-since).UnixMilli()
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, ok, dns_rtt_ms, anchor_rtt_ms, forced_ok, modem_alive, distinct_ok, resolved, detail
		 FROM samples_raw WHERE link_id = ? AND ts >= ? ORDER BY ts`, linkID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Sample
	for rows.Next() {
		var (
			ts                   int64
			ok, fok, alive, ispk int
			dnsMS, anchorMS      sql.NullFloat64
			resolved, detail     string
		)
		if err := rows.Scan(&ts, &ok, &dnsMS, &anchorMS, &fok, &alive, &ispk, &resolved, &detail); err != nil {
			return nil, err
		}
		x := Sample{
			TS: time.UnixMilli(ts), LinkID: linkID, OK: ok == 1,
			ForcedOK: fok == 1, ModemAlive: alive == 1, DistinctOK: ispk == 1,
			Detail: detail,
		}
		if dnsMS.Valid {
			x.DNSRTT = time.Duration(dnsMS.Float64 * float64(time.Millisecond))
		}
		if anchorMS.Valid {
			x.AnchorRTT = time.Duration(anchorMS.Float64 * float64(time.Millisecond))
		}
		if resolved != "" {
			x.Resolved = strings.Split(resolved, ",")
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// RecentSpeeds 返回最近的测速记录。
func (s *Store) RecentSpeeds(ctx context.Context, limit int) ([]SpeedResult, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, link_id, down_mbps, up_mbps, egress_ip, egress_isp, streams, duration_ms, wireless, note
		 FROM speed_tests ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpeedResult
	for rows.Next() {
		var (
			ts, durMS  int64
			wireless   int
			x          SpeedResult
		)
		if err := rows.Scan(&ts, &x.LinkID, &x.DownMbps, &x.UpMbps, &x.EgressIP,
			&x.EgressISP, &x.Streams, &durMS, &wireless, &x.Note); err != nil {
			return nil, err
		}
		x.TS = time.UnixMilli(ts)
		x.Duration = time.Duration(durMS) * time.Millisecond
		x.Wireless = wireless == 1
		out = append(out, x)
	}
	return out, rows.Err()
}

// RecentEvents 返回最近的状态变更事件。
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, link_id, kind, message FROM events ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ts int64
		var e Event
		if err := rows.Scan(&ts, &e.LinkID, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.TS = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Rollup 把已完成的时间桶聚合进 samples_1m / samples_1h，并清理过期原始数据。
// 幂等：重复执行同一时间段不会产生重复行。
func (s *Store) Rollup(ctx context.Context, rawRetain time.Duration) error {
	if err := s.rollupInto(ctx, "samples_1m", time.Minute); err != nil {
		return err
	}
	if err := s.rollupInto(ctx, "samples_1h", time.Hour); err != nil {
		return err
	}
	cutoff := time.Now().Add(-rawRetain).UnixMilli()
	_, err := s.db.ExecContext(ctx, `DELETE FROM samples_raw WHERE ts < ?`, cutoff)
	return err
}

func (s *Store) rollupInto(ctx context.Context, table string, bucketSize time.Duration) error {
	bs := bucketSize.Milliseconds()
	// 只聚合已经结束的桶，避免把当前正在写入的桶算成最终值。
	maxBucket := time.Now().Truncate(bucketSize).UnixMilli()

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT (ts / %d) * %d AS bucket, link_id, ok, dns_rtt_ms
		 FROM samples_raw WHERE ts < ? ORDER BY bucket`, bs, bs), maxBucket)
	if err != nil {
		return err
	}

	type key struct {
		bucket int64
		link   string
	}
	agg := make(map[key]*[]float64)
	cnt := make(map[key][2]int) // n, n_ok

	for rows.Next() {
		var bucket int64
		var link string
		var ok int
		var rtt sql.NullFloat64
		if err := rows.Scan(&bucket, &link, &ok, &rtt); err != nil {
			rows.Close()
			return err
		}
		k := key{bucket, link}
		c := cnt[k]
		c[0]++
		c[1] += ok
		cnt[k] = c
		if rtt.Valid {
			if agg[k] == nil {
				agg[k] = &[]float64{}
			}
			*agg[k] = append(*agg[k], rtt.Float64)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(cnt) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (bucket, link_id, n, n_ok, rtt_min, rtt_p50, rtt_p95, rtt_max)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(bucket, link_id) DO UPDATE SET
		   n=excluded.n, n_ok=excluded.n_ok, rtt_min=excluded.rtt_min,
		   rtt_p50=excluded.rtt_p50, rtt_p95=excluded.rtt_p95, rtt_max=excluded.rtt_max`, table))
	if err != nil {
		return err
	}
	defer stmt.Close()

	for k, c := range cnt {
		var mn, p50, p95, mx any
		if v := agg[k]; v != nil && len(*v) > 0 {
			vs := *v
			sort.Float64s(vs)
			mn, p50, p95, mx = vs[0], percentile(vs, 0.50), percentile(vs, 0.95), vs[len(vs)-1]
		}
		if _, err := stmt.ExecContext(ctx, k.bucket, k.link, c[0], c[1], mn, p50, p95, mx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// percentile 对已排序切片按最近秩法取分位数。
//
// 秩 = ceil(p × N)，转成 0-based 索引即 ceil(p×N)-1。
// 不能写成 int(p × (N-1)) —— 那样会因截断而严重偏低：
// N=2、p=0.95 时 int(0.95×1)=0，P95 会退化成最小值。
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(n))) - 1
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return sorted[i]
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// msOrNil 把时长转成毫秒；零值写入 NULL，以免把"没测到"混同为"0ms"。
func msOrNil(d time.Duration) any {
	if d <= 0 {
		return nil
	}
	return float64(d.Microseconds()) / 1000
}

// LinkStats 是一条线路在某个时间窗口内的质量统计。
type LinkStats struct {
	N       int     `json:"n"`        // 采样总数
	NOK     int     `json:"n_ok"`     // 成功次数
	LossPct float64 `json:"loss_pct"` // 失败占比
	RTTP50  float64 `json:"rtt_p50"`
	RTTP95  float64 `json:"rtt_p95"`
	RTTMax  float64 `json:"rtt_max"`
}

// Stats 统计某条线路最近 since 时间内的质量。
//
// 这里的「失败率」用的是经该线光猫的 DNS 探测成功率，而不是 ICMP 丢包 ——
// ICMP ping 公网目标走哪条 WAN 由路由器的负载均衡决定，主机指定不了，
// 因此它测出来的丢包无法归属到某一条线。而 DNS 探测是确定性地经指定光猫
// 出去的，它的成功率才真正代表这条线的质量。
func (s *Store) Stats(ctx context.Context, linkID string, since time.Duration) (LinkStats, error) {
	from := time.Now().Add(-since).UnixMilli()
	rows, err := s.db.QueryContext(ctx,
		`SELECT ok, dns_rtt_ms FROM samples_raw WHERE link_id = ? AND ts >= ?`, linkID, from)
	if err != nil {
		return LinkStats{}, err
	}
	defer rows.Close()

	var st LinkStats
	var rtts []float64
	for rows.Next() {
		var ok int
		var rtt sql.NullFloat64
		if err := rows.Scan(&ok, &rtt); err != nil {
			return LinkStats{}, err
		}
		st.N++
		st.NOK += ok
		if rtt.Valid && rtt.Float64 > 0 {
			rtts = append(rtts, rtt.Float64)
		}
	}
	if err := rows.Err(); err != nil {
		return LinkStats{}, err
	}
	if st.N > 0 {
		st.LossPct = float64(st.N-st.NOK) / float64(st.N) * 100
	}
	if len(rtts) > 0 {
		sort.Float64s(rtts)
		st.RTTP50 = percentile(rtts, 0.50)
		st.RTTP95 = percentile(rtts, 0.95)
		st.RTTMax = rtts[len(rtts)-1]
	}
	return st, nil
}
