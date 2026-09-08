package probe

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"isp-probe/internal/config"
	"isp-probe/internal/dnsx"
	"isp-probe/internal/netbind"
)

// SpeedProgress 是测速过程中的实时进度，用于面板展示。
type SpeedProgress struct {
	Phase   string  `json:"phase"` // download / upload
	Mbps    float64 `json:"mbps"`  // 瞬时速率
	Elapsed float64 `json:"elapsed_sec"`
	Percent float64 `json:"percent"`
	Warmup  bool    `json:"warmup"` // 是否仍处于预热期
}

// ProgressFunc 接收测速进度。
type ProgressFunc func(SpeedProgress)

// SpeedTester 执行带宽测速。
type SpeedTester struct {
	binder   *netbind.Binder
	resolver *dnsx.Resolver
	cfg      config.SpeedConfig
}

func NewSpeedTester(b *netbind.Binder, r *dnsx.Resolver, cfg config.SpeedConfig) *SpeedTester {
	return &SpeedTester{binder: b, resolver: r, cfg: cfg}
}

// warmupRatio 是丢弃的预热时间占比。
//
// TCP 慢启动期间速率远低于稳态，把这段算进去会显著低估带宽。
// 只统计预热之后的数据。
const warmupRatio = 0.25

// Download 用多条并发连接测量下行带宽。
func (s *SpeedTester) Download(ctx context.Context, onProgress ProgressFunc) (float64, int64, error) {
	if len(s.cfg.DownloadURLs) == 0 {
		return 0, 0, errors.New("未配置下行测速源（speed.download_urls）")
	}

	client := &http.Client{Transport: speedTransport(s.binder, s.resolver, s.cfg.Streams)}
	defer client.CloseIdleConnections()

	var counter atomic.Int64
	runCtx, cancel := context.WithTimeout(ctx, s.cfg.Duration)
	defer cancel()

	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	for i := 0; i < s.cfg.Streams; i++ {
		url := s.cfg.DownloadURLs[i%len(s.cfg.DownloadURLs)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.downloadStream(runCtx, client, url, &counter); err != nil {
				errOnce.Do(func() { firstErr = err })
			}
		}()
	}

	mbps, total := s.sample(runCtx, "download", &counter, onProgress)
	wg.Wait()

	if total == 0 {
		if firstErr == nil {
			firstErr = errors.New("未收到任何数据")
		}
		return 0, 0, fmt.Errorf("下行测速失败: %w", firstErr)
	}
	return mbps, total, nil
}

// downloadStream 持续下载直到 ctx 结束；单条流失败会自动重新发起，
// 以免某个源中途断开导致整体带宽被低估。
func (s *SpeedTester) downloadStream(ctx context.Context, client *http.Client, url string, counter *atomic.Int64) error {
	var lastErr error
	for ctx.Err() == nil {
		req, err := newRequest(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Cache-Control", "no-cache")

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				// 正常收尾；但仍要把最后一次真实错误带回去，
				// 否则「一直失败」会被误报成「没收到数据」而无从排查。
				return lastErr
			}
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			return fmt.Errorf("%s 返回 HTTP %d", url, resp.StatusCode)
		}
		_, err = io.Copy(&countingWriter{n: counter}, resp.Body)
		resp.Body.Close()
		if err != nil && ctx.Err() == nil {
			lastErr = err
		}
	}
	return lastErr
}

// Upload 用多条并发连接测量上行带宽。
func (s *SpeedTester) Upload(ctx context.Context, onProgress ProgressFunc) (float64, int64, error) {
	if s.cfg.UploadURL == "" {
		return 0, 0, errors.New("未配置上行测速端点（speed.upload_url）")
	}

	client := &http.Client{Transport: speedTransport(s.binder, s.resolver, s.cfg.Streams)}
	defer client.CloseIdleConnections()

	var counter atomic.Int64
	runCtx, cancel := context.WithTimeout(ctx, s.cfg.Duration)
	defer cancel()

	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	for i := 0; i < s.cfg.Streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.uploadStream(runCtx, client, &counter); err != nil {
				errOnce.Do(func() { firstErr = err })
			}
		}()
	}

	mbps, total := s.sample(runCtx, "upload", &counter, onProgress)
	wg.Wait()

	if total == 0 {
		if firstErr == nil {
			firstErr = errors.New("未发出任何数据")
		}
		return 0, 0, fmt.Errorf("上行测速失败: %w", firstErr)
	}
	return mbps, total, nil
}

// uploadChunk 是单次上传请求的大小。
//
// 必须按固定大小分块、并设置 Content-Length：如果给 body 一个长度未知的
// reader，Go 会改用 chunked 传输编码，不少服务端（含 Cloudflare 的测速
// 端点）对此不接受，表现为请求发不出去、计数始终为零。
const uploadChunk = 8 << 20

func (s *SpeedTester) uploadStream(ctx context.Context, client *http.Client, counter *atomic.Int64) error {
	var lastErr error
	for ctx.Err() == nil {
		body := io.LimitReader(&randomReader{counter: counter}, uploadChunk)
		req, err := newRequest(ctx, http.MethodPost, s.cfg.UploadURL, body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = uploadChunk

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return lastErr
			}
			lastErr = err
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
	return lastErr
}

// sample 周期性采样计数器，推送进度，并在结束时算出稳态速率。
//
// 返回的速率只统计预热期之后的数据 —— 把 TCP 慢启动算进去会明显低估带宽。
func (s *SpeedTester) sample(ctx context.Context, phase string, counter *atomic.Int64, onProgress ProgressFunc) (float64, int64) {
	const tick = 250 * time.Millisecond
	start := time.Now()
	warmup := time.Duration(float64(s.cfg.Duration) * warmupRatio)

	var (
		warmupBytes int64
		warmupAt    time.Time
		warmedUp    bool
		lastBytes   int64
		lastAt      = start
	)

	t := time.NewTicker(tick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			total := counter.Load()
			if !warmedUp {
				// 整个过程都没跑完预热期，只能用全程平均值。
				elapsed := time.Since(start).Seconds()
				if elapsed <= 0 {
					return 0, total
				}
				return toMbps(total, elapsed), total
			}
			elapsed := time.Since(warmupAt).Seconds()
			if elapsed <= 0 {
				return 0, total
			}
			return toMbps(total-warmupBytes, elapsed), total

		case now := <-t.C:
			cur := counter.Load()
			elapsed := now.Sub(start)

			if !warmedUp && elapsed >= warmup {
				warmedUp = true
				warmupBytes = cur
				warmupAt = now
			}

			if onProgress != nil {
				inst := toMbps(cur-lastBytes, now.Sub(lastAt).Seconds())
				onProgress(SpeedProgress{
					Phase:   phase,
					Mbps:    inst,
					Elapsed: elapsed.Seconds(),
					Percent: min(100, elapsed.Seconds()/s.cfg.Duration.Seconds()*100),
					Warmup:  !warmedUp,
				})
			}
			lastBytes, lastAt = cur, now
		}
	}
}

func toMbps(bytes int64, seconds float64) float64 {
	if seconds <= 0 || bytes <= 0 {
		return 0
	}
	return float64(bytes) * 8 / seconds / 1e6
}

// countingWriter 统计写入字节数后丢弃数据。
type countingWriter struct{ n *atomic.Int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n.Add(int64(len(p)))
	return len(p), nil
}

// randomReader 持续产生随机数据直到 ctx 结束，用作上行测速的请求体。
//
// 用随机数据而非全零，是为了避免中间链路或服务端的压缩把结果虚高。
type randomReader struct {
	counter *atomic.Int64
	buf     []byte
}

func (r *randomReader) Read(p []byte) (int, error) {
	if r.buf == nil {
		// 复用一块随机数据即可：目的是压不掉，不需要每次都重新生成。
		r.buf = make([]byte, 64*1024)
		rand.Read(r.buf)
	}
	n := copy(p, r.buf)
	r.counter.Add(int64(n))
	return n, nil
}
