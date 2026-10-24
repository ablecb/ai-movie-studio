// 运行目录互斥锁：防止两个进程同时操作同一个 run。
//
// 真实事故场景：一个 render 还在写 output/shots/sh003.mp4，另一个 render
// 的断点复用把它当成"已完成产物"——半截文件混进成片。state.json 有原子写，
// 但产物文件没有"事务"可言，唯一可靠的防线就是不并发。
package fsx

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// lockStaleAfter 超过该时长的锁文件视为残留（进程崩溃没清理且 PID 无法解析时），
// 允许接管。媒体渲染的最长单镜头超时是分钟级，6 小时远大于任何合法持有场景。
const lockStaleAfter = 6 * time.Hour

// LockRun 对运行目录加互斥锁。返回释放函数；失败返回错误（带处置指引）。
func LockRun(runDir string) (func(), error) {
	lockPath := filepath.Join(runDir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("创建运行锁失败: %w", err)
		}
		if lockIsStale(lockPath) {
			// 残留锁：持有进程已退出（取消任务/崩溃时 deferred 释放不会执行）。
			// 接管而不是让用户手工解锁。
			os.Remove(lockPath)
			return LockRun(runDir)
		}
		return nil, fmt.Errorf(
			"该 run 正被另一个进程使用（锁文件 %s）。\n"+
				"  同一 run 并发渲染会互相破坏产物（断点复用会把半截文件当成品）。"+
				"确认没有并发操作后删除该锁文件重试", lockPath)
	}
	fmt.Fprintf(f, "pid=%d\nat=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	f.Close()
	return func() { os.Remove(lockPath) }, nil
}

// lockIsStale 判断既有锁是否可安全接管：持有进程已退出 → 是；
// 锁内容损坏/无 PID 时退回按 mtime 的 6 小时兜底。
func lockIsStale(lockPath string) bool {
	if data, err := os.ReadFile(lockPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if pidStr, ok := strings.CutPrefix(line, "pid="); ok {
				if pid, perr := strconv.Atoi(pidStr); perr == nil {
					return !processAlive(pid)
				}
			}
		}
	}
	if info, statErr := os.Stat(lockPath); statErr == nil {
		return time.Since(info.ModTime()) > lockStaleAfter
	}
	return false
}

// processAlive 进程是否存活。Windows 上 FindProcess 内部走 OpenProcess，
// 进程不存在会直接报错；Unix 再用 Signal(0) 探活。
// 注意 PID 复用可能造成误判"存活"，概率低且后果只是要求人工解锁，可接受。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
