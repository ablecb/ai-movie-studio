package fsx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLockRunExclusive(t *testing.T) {
	dir := t.TempDir()
	release, err := LockRun(dir)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	if _, err := LockRun(dir); err == nil {
		t.Fatal("持锁期间第二次加锁应当失败")
	}
	release()
	if _, err := LockRun(dir); err != nil {
		t.Fatalf("释放后应可重新加锁: %v", err)
	}
}

func TestLockStealFromDeadProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")

	// 起一个立即退出的进程，拿到一个确定已死的 PID
	cmd := exec.Command("cmd", "/c", "exit")
	if err := cmd.Start(); err != nil {
		t.Skipf("无法启动测试进程: %v", err)
	}
	deadPID := cmd.Process.Pid
	cmd.Wait() // 已退出

	if err := os.WriteFile(lockPath, []byte("pid="+strconv.Itoa(deadPID)+"\nat=2020-01-01T00:00:00+08:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lockIsStale(lockPath) != true {
		t.Fatal("死进程持有的锁应判定为残留")
	}
	release, err := LockRun(dir)
	if err != nil {
		t.Fatalf("死进程的锁应被自动接管: %v", err)
	}
	release()
}

func TestLockHeldByLiveProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")
	if err := os.WriteFile(lockPath, []byte("pid="+strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lockIsStale(lockPath) {
		t.Fatal("本进程（存活）持有的锁不应判定为残留")
	}
	if _, err := LockRun(dir); err == nil {
		t.Fatal("存活进程持锁时加锁应失败")
	}
}

func TestLockStaleByAge(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")
	// 无 PID 的旧格式锁：按 mtime 兜底
	if err := os.WriteFile(lockPath, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-7 * time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	if !lockIsStale(lockPath) {
		t.Fatal("超过 6 小时的无主锁应判定为残留")
	}
	if _, err := LockRun(dir); err != nil {
		t.Fatalf("残留锁应被接管: %v", err)
	}
}
