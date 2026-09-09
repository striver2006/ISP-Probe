// Package service 负责把 ISP探针 安装为随登录/开机自启的后台服务。
//
// 两个平台的模型差别很大（launchd 是用户会话里的作业，Windows SCM 是
// session 0 的系统服务），公共层只抽出「装/卸/启/停/查」五个动作和一份
// 与平台无关的安装参数；plist 与 SCM 的细节各自封在 build tag 文件里。
//
// 与项目其余部分一致，只实现 darwin 与 windows —— 不给 linux 留 stub，
// 免得造出「能编译但跑不了」的假象。
package service

import (
	"context"
	"log/slog"
)

const (
	// DarwinLabel 既是 LaunchAgent 的 Label，也是 plist 的文件名。
	DarwinLabel = "com.czb.isp-probe"

	// WindowsName 是 SCM 里的服务名（内部键，不含空格与中文）。
	WindowsName        = "ISPProbe"
	WindowsDisplayName = "ISP探针 (ISP-Probe)"
	WindowsDescription = "双线宽带接入点监测：定时探测各 WAN 连通性，异常时告警，并提供本地面板"

	// EnvManaged 由服务管理器注入，让进程知道自己不在前台。
	// Windows 上用 svc.IsWindowsService() 判断更可靠，这个变量只用于 macOS。
	EnvManaged = "ISP_PROBE_SERVICE"
)

// Spec 是安装一个服务实例所需的全部参数，全部为绝对路径。
type Spec struct {
	Exe     string // 可执行文件绝对路径，已解 symlink
	Cfg     string // 配置文件绝对路径
	WorkDir string // 工作目录，通常是配置文件所在目录
	LogDir  string // 日志目录
	Listen  string // 面板监听地址，用于安装后校验
}

// Status 是服务的当前状态快照。
type Status struct {
	Installed bool
	Running   bool
	PID       int
	State     string // 人类可读："运行中" / "已停止" / "未安装"
	ExePath   string // 已登记的可执行文件路径，用于发现「装的是旧位置的二进制」
	CfgPath   string // 已登记的 -c 参数
	Detail    string // 平台原生输出的摘要，排障用
}

// Manager 管理本平台的服务生命周期。除 Install 外，所有方法都不需要
// 加载配置、也不需要探测物理网卡。
type Manager interface {
	Install(ctx context.Context, s Spec) error
	Uninstall(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (Status, error)
}

// New 返回当前平台的服务管理器。
func New(log *slog.Logger) (Manager, error) { return newManager(log) }

// Runner 是被托管运行的主体。
//
// 必须在真正可对外服务之后调用 ready()：Windows SCM 据此把服务状态从
// StartPending 切到 Running，迟迟不切会被判定为启动失败。
type Runner func(ctx context.Context, ready func()) error

// IsManaged 报告当前进程是否由服务管理器拉起。
func IsManaged() bool { return isManaged() }

// RunManaged 在服务托管语义下运行 fn：
// Windows 走 SCM 调度循环，macOS 走 SIGTERM 信号。
func RunManaged(fn Runner) error { return runManaged(fn) }

// RequirePrivilege 在权限不足以安装/卸载服务时返回带操作指引的错误。
func RequirePrivilege() error { return requirePrivilege() }
