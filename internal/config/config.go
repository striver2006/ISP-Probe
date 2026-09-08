// Package config 负责加载与校验配置。
//
// 设计原则：所有与具体环境相关的值（路由器地址、光猫 IP、WAN 策略名、
// 运营商归属关键字）全部放配置文件，代码里不出现任何硬编码地址，
// 换设备或换到别人家只改 YAML。
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
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
	UploadURL string `yaml:"upload_url"`
	Streams   int    `yaml:"streams"`  // 并发连接数，跑满千兆需要多流
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
}

type WebConfig struct {
	Listen string `yaml:"listen"`
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
			UploadURL:  "https://speed.cloudflare.com/__up",
			EgressURLs: []string{
				"https://myip.ipip.net",
				"https://cip.cc",
				"http://ip-api.com/json/?fields=query,isp,org,as",
			},
		},
		Store:  StoreConfig{Path: "isp-probe.db", RawRetain: 7 * 24 * time.Hour},
		Notify: NotifyConfig{Desktop: true},
		Web:    WebConfig{Listen: "127.0.0.1:8686"},
	}
}

// Load 读取配置文件；文件不存在时返回默认配置。
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, cfg.validate()
		}
		return cfg, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	return cfg, cfg.validate()
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
