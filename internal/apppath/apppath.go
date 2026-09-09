// Package apppath 负责定位可执行文件、配置文件，以及把配置里的相对路径
// 锚定到一个稳定的基准目录。
//
// 存在的理由：程序既要能在仓库目录里前台跑（cwd == 可执行文件目录），
// 也要能被 launchd / Windows SCM 拉起 —— 后者的 cwd 通常是 `/` 或
// C:\Windows\System32，此时任何相对路径都会指向意料之外的位置。
package apppath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigName 是未显式指定 -c 时查找的文件名。
const ConfigName = "config.yaml"

// Paths 是一次路径发现的结果。
type Paths struct {
	Exe    string // 可执行文件绝对路径，已解 symlink
	ExeDir string // Exe 所在目录
	CWD    string // 进程启动时的工作目录
	// Base 是数据与日志的锚点，等于配置文件所在目录。
	// FindConfig 成功后才有值；失败时回落到 ExeDir。
	Base string
}

// Discover 采集当前进程的路径信息。任何一项失败都只是留空，不返回错误 ——
// 调用方总能靠剩下的信息继续工作。
func Discover() Paths {
	var p Paths
	p.CWD, _ = os.Getwd()

	exe, err := os.Executable()
	if err != nil {
		p.Base = p.CWD
		return p
	}
	// EvalSymlinks 必须做：装到 /usr/local/bin 之类的位置常常是一条软链，
	// 而 launchd plist 的 ProgramArguments 需要真实路径才不会在升级后失效。
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	p.Exe, p.ExeDir = exe, filepath.Dir(exe)
	p.Base = p.ExeDir
	return p
}

// IsTempBuild 判断可执行文件是否是 `go run` 产生的临时二进制。
//
// 这种二进制退出即被删除，把它的路径写进 plist 或服务 ImagePath 必然失效，
// 所以 service install 要据此拒绝。
func (p Paths) IsTempBuild() bool {
	return p.ExeDir != "" && strings.Contains(filepath.ToSlash(p.ExeDir), "/go-build")
}

// NotFoundError 描述一次失败的配置查找，携带所有尝试过的路径。
type NotFoundError struct {
	Tried  []string
	Labels []string // 与 Tried 一一对应的来源说明
}

func (e *NotFoundError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "找不到配置文件 %s。已尝试:\n", ConfigName)
	for i, t := range e.Tried {
		fmt.Fprintf(&b, "  %s   (%s)\n", t, e.Labels[i])
	}
	b.WriteString("\n用 -c <路径> 指定，或复制一份 config.yaml 放到可执行文件旁边。")
	return b.String()
}

// FindConfig 定位配置文件并把 Base 设为它所在的目录。
//
// explicit 非空时按 shell 语义相对 cwd 解析，不去猜可执行文件目录 ——
// 用户明明写了 ./a.yaml 却读到别处，是最难排查的一类问题。
// explicit 为空时依次尝试 cwd 与可执行文件目录：cwd 优先保证仓库里
// `./isp-probe serve` 和 `go run ./cmd/isp-probe` 行为不变，
// 可执行文件目录兜底实现「整个文件夹拷走就能用」。
func (p *Paths) FindConfig(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("解析配置路径 %s 失败: %w", explicit, err)
		}
		if st, err := os.Stat(abs); err != nil || st.IsDir() {
			return "", fmt.Errorf("配置文件不存在: %s", abs)
		}
		p.Base = filepath.Dir(abs)
		return abs, nil
	}

	var tried, labels []string
	add := func(dir, label string) {
		if dir == "" {
			return
		}
		c := filepath.Join(dir, ConfigName)
		for _, t := range tried {
			if t == c {
				return
			}
		}
		tried, labels = append(tried, c), append(labels, label)
	}
	add(p.CWD, "当前目录")
	add(p.ExeDir, "可执行文件所在目录")

	for _, c := range tried {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			p.Base = filepath.Dir(c)
			return c, nil
		}
	}
	return "", &NotFoundError{Tried: tried, Labels: labels}
}
