// Package config 负责加载与校验配置。
//
// 设计原则：所有与具体环境相关的值（路由器地址、光猫 IP、WAN 策略名、
// 运营商归属关键字）全部放配置文件，代码里不出现任何硬编码地址，
// 换设备或换到别人家只改 YAML。
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Iface  IfaceConfig  `yaml:"iface"`
	Router RouterConfig `yaml:"router"`
	Links  []LinkConfig `yaml:"links"`
	Probe  ProbeConfig  `yaml:"probe"`
	Speed  SpeedConfig  `yaml:"speed"`
	Store  StoreConfig  `yaml:"store"`
	Notify NotifyConfig `yaml:"notify"`
	Web    WebConfig    `yaml:"web"`
	Log    LogConfig    `yaml:"log"`
}

type IfaceConfig struct {
	// Name 留空则自动识别物理网卡；填 "en0" 之类可强制指定。
	Name string `yaml:"name"`
	// BindLocalAddr 额外绑定源地址做双保险，默认关闭
	//（接口索引绑定已足够，且对 DHCP 换 IP 免疫）。
	BindLocalAddr bool `yaml:"bind_local_addr"`
}

type RouterConfig struct {
	// AdminURL 是面板上可点击的路由器管理页链接。留空则用默认路由网关。
	AdminURL string `yaml:"admin_url"`
	// RulePageHint 是引导文案里的菜单名，例如「WAN口转发规则设置」。
	RulePageHint string `yaml:"rule_page_hint"`
}

// LinkConfig 描述一条 WAN 线路。
type LinkConfig struct {
	ID   string `yaml:"id"`   // 内部标识，如 telecom
	Name string `yaml:"name"` // 展示名，如 电信
	// WANLabel 是引导用户在路由器上选择的策略名，如「WAN1优先」。
	WANLabel string `yaml:"wan_label"`
	// ModemIP 是该线路光猫的 LAN IP，分线 DNS 探测的入口。
	ModemIP string `yaml:"modem_ip"`
	// ExpectISP 是出口归属校验关键字（小写匹配），用于确认流量确实走了这条线。
	ExpectISP []string `yaml:"expect_isp"`
	// ModemPorts 用于光猫存活检测，留空则用默认端口。
	ModemPorts []int `yaml:"modem_ports"`

	modemAddr netip.Addr
}

// ModemAddr 返回已解析的光猫地址。
func (l *LinkConfig) ModemAddr() netip.Addr { return l.modemAddr }

// MatchISP 判断一个运营商描述串是否属于本线路。
func (l *LinkConfig) MatchISP(isp string) bool {
	s := strings.ToLower(isp)
	for _, kw := range l.ExpectISP {
		if kw != "" && strings.Contains(s, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

type ProbeConfig struct {
	Interval time.Duration `yaml:"interval"` // 每条线的探测间隔
	MinGap   time.Duration `yaml:"min_gap"`  // 同一光猫两次查询的最小间隔，防触发限速
	Timeout  time.Duration `yaml:"timeout"`  // 单次 DNS 查询超时
	// FailThreshold 连续失败多少次才判定为故障并告警，用于抑制抖动误报。
	FailThreshold int `yaml:"fail_threshold"`
	// RetrySlow 判定故障前的降速重试次数，用于排除光猫限速。
	RetrySlow int `yaml:"retry_slow"`
	// CacheDomain 用于测缓存响应延迟；ForceDomain 的随机子域名用于强制递归。
	CacheDomain string `yaml:"cache_domain"`
	ForceDomain string `yaml:"force_domain"`
	// Anchors 是 TCP connect 延迟锚点，必须是 IP:port 形式。
	Anchors []string `yaml:"anchors"`
}

type SpeedConfig struct {
	// DownloadURLs 下行测速源，需支持大文件与 Range。
	DownloadURLs []string `yaml:"download_urls"`
	// UploadURL 上行测速端点（接受 POST 并丢弃 body）。
	UploadURL string        `yaml:"upload_url"`
	Streams   int           `yaml:"streams"`  // 并发连接数，跑满千兆需要多流
	Duration  time.Duration `yaml:"duration"` // 单向测速时长
	// EgressURLs 查询出口 IP 与运营商归属的服务，多源交叉。
	EgressURLs []string `yaml:"egress_urls"`
}

type StoreConfig struct {
	Path      string        `yaml:"path"`       // SQLite 文件路径
	RawRetain time.Duration `yaml:"raw_retain"` // 原始采样保留时长
}

type NotifyConfig struct {
	Desktop bool `yaml:"desktop"` // 系统桌面通知
	// RepeatAlert 控制持续故障是否重复提醒。关掉则退化成「状态跃迁各发一次」。
	RepeatAlert bool            `yaml:"repeat_alert"`
	Webhooks    []WebhookConfig `yaml:"webhooks"`
}

// 支持的 IM 机器人类型。
const (
	KindWeCom    = "wecom"
	KindDingTalk = "dingtalk"
	KindFeishu   = "feishu"
	KindCustom   = "custom"
)

// WebhookKinds 是全部合法的 kind，供校验与错误提示复用。
var WebhookKinds = []string{KindWeCom, KindDingTalk, KindFeishu, KindCustom}

// WebhookConfig 是一个 IM 群机器人。
//
// URL 里通常带着 key/token，等同于密钥 —— 不要写进随仓库提交的 config.yaml，
// 放到同目录下不被跟踪的 config.local.yaml 里。
type WebhookConfig struct {
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"` // wecom | dingtalk | feishu | custom
	URL     string `yaml:"url"`
	Secret  string `yaml:"secret"` // 钉钉/飞书加签用；企微不需要
	Enabled bool   `yaml:"enabled"`
	// Events 是订阅的事件类型，留空即全部（down / up / down_repeat）。
	Events  []string      `yaml:"events"`
	Timeout time.Duration `yaml:"timeout"`
	// 以下两项仅 kind=custom 有意义。
	ContentType  string `yaml:"content_type"`
	BodyTemplate string `yaml:"body_template"`
}

type WebConfig struct {
	Listen string `yaml:"listen"`
	// Token 非空时，来自非环回地址的请求必须带上它才能访问接口。
	// 留空 = 不鉴权（面板只监听 127.0.0.1 时的既有行为）。
	Token string `yaml:"token"`
}

// LogConfig 控制诊断日志的去向与轮转。
//
// File 留空时：前台运行保持输出到 stderr（行为与服务化之前完全一致），
// 服务模式则自动落到 <配置目录>/logs/isp-probe.log —— 后台进程的 stderr
// 在 Windows 服务里等于丢弃，必须落文件才有排障可能。
type LogConfig struct {
	File      string `yaml:"file"`
	MaxSizeMB int    `yaml:"max_size_mb"` // 单文件上限，超过即轮转
	Keep      int    `yaml:"keep"`        // 保留几个历史文件
}

// Default 返回内置默认值，对应本机实测得到的拓扑。
func Default() Config {
	return Config{
		Router: RouterConfig{RulePageHint: "WAN口转发规则设置"},
		Probe: ProbeConfig{
			Interval:      60 * time.Second,
			MinGap:        2 * time.Second,
			Timeout:       5 * time.Second,
			FailThreshold: 3,
			RetrySlow:     3,
			CacheDomain:   "www.baidu.com",
			ForceDomain:   "example.com",
			Anchors:       []string{"223.5.5.5:443", "119.29.29.29:443"},
		},
		Speed: SpeedConfig{
			Streams:  6,
			Duration: 12 * time.Second,
			DownloadURLs: []string{
				"https://mirrors.tuna.tsinghua.edu.cn/ubuntu-releases/20.04.6/ubuntu-20.04.6-desktop-amd64.iso",
				"https://mirrors.ustc.edu.cn/ubuntu-releases/20.04.6/ubuntu-20.04.6-desktop-amd64.iso",
				"https://mirrors.aliyun.com/ubuntu-releases/20.04.6/ubuntu-20.04.6-desktop-amd64.iso",
			},
			UploadURL: "https://speed.cloudflare.com/__up",
			EgressURLs: []string{
				"https://myip.ipip.net",
				"https://cip.cc",
				"http://ip-api.com/json/?fields=query,isp,org,as",
			},
		},
		Store:  StoreConfig{Path: "isp-probe.db", RawRetain: 7 * 24 * time.Hour},
		Notify: NotifyConfig{Desktop: true, RepeatAlert: true},
		Web:    WebConfig{Listen: "127.0.0.1:8686"},
		Log:    LogConfig{MaxSizeMB: 8, Keep: 3},
	}
}

// ErrConfigNotFound 让调用方能区分「文件不存在」与「内容有问题」。
var ErrConfigNotFound = errors.New("配置文件不存在")

// Load 读取并校验配置。path 必须是已经定位好的真实路径。
//
// 相对形式的 store.path / log.file 会被锚定到 path 所在目录，使得配置、
// 数据库、日志能作为一个整体搬走 —— 服务模式下 cwd 不由我们决定，
// 相对路径若不锚定就会落到 `/` 或 System32。
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 这里曾经静默返回 Default()，而 Default().Links 为空，
			// 用户看到的报错是「配置中未定义任何线路」—— 与真实原因南辕北辙。
			return cfg, fmt.Errorf("%w: %s", ErrConfigNotFound, path)
		}
		return cfg, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	if err := mergeLocal(&cfg, filepath.Join(filepath.Dir(path), LocalName)); err != nil {
		return cfg, err
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	cfg.Anchor(filepath.Dir(path))
	return cfg, nil
}

// LocalName 是旁路配置的文件名。
//
// 存在的理由：config.yaml 随仓库提交，而 webhook URL 里的 key 是密钥。
// 旁路文件不进 git（见 .gitignore），只写需要覆盖的字段。
const LocalName = "config.local.yaml"

// mergeLocal 把旁路配置叠加到 cfg 上。
//
// 叠加语义就是 yaml.v3 对同一个 struct 反序列化两次的语义：出现的字段覆盖，
// 未出现的保留。注意**切片是整体替换而不是追加** —— 旁路里写了 webhooks
// 就是全量覆盖主配置里那份，不会合并。
//
// 文件不存在是正常情况（多数人不配 IM 通知），静默跳过；但存在却解析失败
// 必须报错：静默忽略会让人以为密钥已经生效，实际一条告警都发不出去。
func mergeLocal(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取旁路配置 %s 失败: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("解析旁路配置 %s 失败: %w", path, err)
	}
	return nil
}

// Anchor 把相对路径锚定到 base。重复调用是安全的：已绝对化的值不再变动。
func (c *Config) Anchor(base string) {
	if base == "" {
		return
	}
	fix := func(s *string) {
		if *s != "" && !filepath.IsAbs(*s) {
			*s = filepath.Join(base, *s)
		}
	}
	fix(&c.Store.Path)
	fix(&c.Log.File)
}

func (c *Config) validate() error {
	if len(c.Links) == 0 {
		return fmt.Errorf("配置中未定义任何线路（links）")
	}
	seen := make(map[string]bool)
	for i := range c.Links {
		l := &c.Links[i]
		if l.ID == "" {
			return fmt.Errorf("第 %d 条线路缺少 id", i+1)
		}
		if seen[l.ID] {
			return fmt.Errorf("线路 id 重复: %s", l.ID)
		}
		seen[l.ID] = true
		if l.Name == "" {
			l.Name = l.ID
		}
		addr, err := netip.ParseAddr(l.ModemIP)
		if err != nil {
			return fmt.Errorf("线路 %s 的 modem_ip %q 不是合法 IP: %w", l.ID, l.ModemIP, err)
		}
		l.modemAddr = addr
	}
	for _, a := range c.Probe.Anchors {
		if _, err := netip.ParseAddrPort(a); err != nil {
			return fmt.Errorf("anchor %q 必须是 IP:port 形式（不能用域名，会走系统解析器）: %w", a, err)
		}
	}
	if c.Probe.Interval <= 0 {
		c.Probe.Interval = 60 * time.Second
	}
	if c.Probe.FailThreshold <= 0 {
		c.Probe.FailThreshold = 3
	}
	if c.Speed.Streams <= 0 {
		c.Speed.Streams = 6
	}
	if c.Log.MaxSizeMB <= 0 {
		c.Log.MaxSizeMB = 8
	}
	if c.Log.Keep <= 0 {
		c.Log.Keep = 3
	}
	if err := c.validateWebhooks(); err != nil {
		return err
	}
	return nil
}

// validateWebhooks 校验通知渠道。
//
// 这里对 custom 模板做一次试解析：模板写错是配置问题，应该在启动时就报出来，
// 而不是等到线路真的断了、最需要告警的那一刻才发现发不出去。
func (c *Config) validateWebhooks() error {
	for i := range c.Notify.Webhooks {
		w := &c.Notify.Webhooks[i]
		label := w.Name
		if label == "" {
			label = fmt.Sprintf("第 %d 个", i+1)
		}
		if !slices.Contains(WebhookKinds, w.Kind) {
			return fmt.Errorf("通知渠道 %s 的 kind %q 无效，可选：%s",
				label, w.Kind, strings.Join(WebhookKinds, " / "))
		}
		if w.Name == "" {
			w.Name = w.Kind
		}
		u, err := url.Parse(w.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("通知渠道 %s 的 url 不是合法的 http(s) 地址: %q", w.Name, w.URL)
		}
		if w.Kind == KindCustom {
			if w.BodyTemplate == "" {
				return fmt.Errorf("通知渠道 %s 是 custom 类型，必须提供 body_template", w.Name)
			}
			if _, err := template.New("webhook").Parse(w.BodyTemplate); err != nil {
				return fmt.Errorf("通知渠道 %s 的 body_template 语法有误: %w", w.Name, err)
			}
		}
		if w.Timeout <= 0 {
			w.Timeout = 8 * time.Second
		}
	}
	return nil
}

// Link 按 id 查找线路。
func (c *Config) Link(id string) *LinkConfig {
	for i := range c.Links {
		if c.Links[i].ID == id {
			return &c.Links[i]
		}
	}
	return nil
}
