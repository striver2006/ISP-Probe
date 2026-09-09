package apppath

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, ConfigName)
	if err := os.WriteFile(p, []byte("links: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFindConfigPrefersCWD 保证仓库里 ./isp-probe serve 的既有行为不变：
// 当前目录有配置就用它，不去可执行文件目录找。
func TestFindConfigPrefersCWD(t *testing.T) {
	cwd, exe := t.TempDir(), t.TempDir()
	want := writeCfg(t, cwd)
	writeCfg(t, exe)

	p := Paths{CWD: cwd, ExeDir: exe}
	got, err := p.FindConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("应优先取当前目录的 %s，实得 %s", want, got)
	}
	if p.Base != cwd {
		t.Errorf("Base 应为配置所在目录 %s，实得 %s", cwd, p.Base)
	}
}

// TestFindConfigFallsBackToExeDir 是「绿色便携」的依据：
// 服务模式下 cwd 不由我们决定，必须能回落到二进制旁边。
func TestFindConfigFallsBackToExeDir(t *testing.T) {
	cwd, exe := t.TempDir(), t.TempDir()
	want := writeCfg(t, exe)

	p := Paths{CWD: cwd, ExeDir: exe}
	got, err := p.FindConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("应回落到可执行文件目录的 %s，实得 %s", want, got)
	}
}

// TestFindConfigExplicitDoesNotGuess 保证显式 -c 按 shell 语义解析：
// 用户写了路径却读到别处，是最难排查的一类问题。
func TestFindConfigExplicitDoesNotGuess(t *testing.T) {
	cwd, exe := t.TempDir(), t.TempDir()
	writeCfg(t, exe) // 可执行文件目录里有配置，但显式路径指向别处

	p := Paths{CWD: cwd, ExeDir: exe}
	if _, err := p.FindConfig(filepath.Join(cwd, "missing.yaml")); err == nil {
		t.Error("显式指定的路径不存在时应报错，而不是回落到其它目录")
	}
}

// TestNotFoundListsAllCandidates 保证报错文案把试过的路径都列出来 ——
// 原先的实现在这里报的是「未定义任何线路」，与真实原因南辕北辙。
func TestNotFoundListsAllCandidates(t *testing.T) {
	cwd, exe := t.TempDir(), t.TempDir()
	p := Paths{CWD: cwd, ExeDir: exe}
	_, err := p.FindConfig("")
	if err == nil {
		t.Fatal("两个目录都没有配置时应报错")
	}
	nf, ok := err.(*NotFoundError)
	if !ok {
		t.Fatalf("应返回 *NotFoundError，实得 %T", err)
	}
	if len(nf.Tried) != 2 {
		t.Errorf("应列出 2 个候选路径，实得 %d: %v", len(nf.Tried), nf.Tried)
	}
}

// TestFindConfigDedupes 覆盖最常见的情形：cwd 与可执行文件目录相同，
// 此时不该把同一个路径报两遍。
func TestFindConfigDedupes(t *testing.T) {
	dir := t.TempDir()
	p := Paths{CWD: dir, ExeDir: dir}
	_, err := p.FindConfig("")
	nf, ok := err.(*NotFoundError)
	if !ok {
		t.Fatalf("应返回 *NotFoundError，实得 %v", err)
	}
	if len(nf.Tried) != 1 {
		t.Errorf("同一目录应只列一次，实得 %v", nf.Tried)
	}
}

func TestIsTempBuild(t *testing.T) {
	for _, c := range []struct {
		dir  string
		want bool
	}{
		{"/var/folders/xy/T/go-build123/b001/exe", true},
		{"/Users/me/Work/ISP-Probe", false},
	} {
		if got := (Paths{ExeDir: c.dir}).IsTempBuild(); got != c.want {
			t.Errorf("IsTempBuild(%s) = %v, want %v", c.dir, got, c.want)
		}
	}
}
