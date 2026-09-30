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
	"time"
)

// lockStaleAfter 超过该时长的锁文件视为残留（进程崩溃没清理），允许接管。
// 媒体渲染的最长单镜头超时是分钟级，6 小时远大于任何合法持有场景。
const lockStaleAfter = 6 * time.Hour

// LockRun 对运行目录加互斥锁。返回释放函数；失败返回错误（带处置指引）。
func LockRun(runDir string) (func(), error) {
	lockPath := filepath.Join(runDir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("创建运行锁失败: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			// 残留锁：上次进程崩溃未清理。接管而不是让用户手工解锁。
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
