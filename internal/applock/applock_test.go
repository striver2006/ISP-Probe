package applock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSecondAcquireFails 验证同一路径的第二次加锁会被拒绝。
func TestSecondAcquireFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("首次加锁应成功: %v", err)
	}
	defer l.Release()

	if _, err := Acquire(p); !errors.Is(err, ErrLocked) {
		t.Errorf("第二次加锁应返回 ErrLocked，实得 %v", err)
	}
}

// TestReleaseAllowsReacquire 验证释放后能重新加锁，且重复 Release 安全。
func TestReleaseAllowsReacquire(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("重复 Release 应当无害，实得 %v", err)
	}

	l2, err := Acquire(p)
	if err != nil {
		t.Fatalf("释放后应能重新加锁: %v", err)
	}
	l2.Release()
}

// TestLockReleasedOnProcessExit 是不用「PID 文件 + 判进程存活」的依据：
// 锁由内核在进程结束时释放，因此不存在陈旧锁。
func TestLockReleasedOnProcessExit(t *testing.T) {
	if os.Getenv("APPLOCK_CHILD") != "" {
		l, err := Acquire(os.Getenv("APPLOCK_PATH"))
		if err != nil {
			os.Exit(3)
		}
		_ = l
		os.Exit(0) // 故意不 Release，让内核回收
	}

	p := filepath.Join(t.TempDir(), "x.lock")
	cmd := exec.Command(os.Args[0], "-test.run=TestLockReleasedOnProcessExit")
	cmd.Env = append(os.Environ(), "APPLOCK_CHILD=1", "APPLOCK_PATH="+p)
	if err := cmd.Run(); err != nil {
		t.Fatalf("子进程加锁失败: %v", err)
	}

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("子进程退出后锁应已释放，实得 %v", err)
	}
	l.Release()
}
