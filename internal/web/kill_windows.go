//go:build windows

package web

import (
	"os/exec"
	"strconv"
)

// applySysProcAttr Windows 下无需进程组；整树终止交给 taskkill /T。
func applySysProcAttr(cmd *exec.Cmd) {}

// killProcessTree 终止进程及其全部子进程（ffmpeg/ffprobe 都可能在跑）。
// taskkill /T 连树一起杀；/F 强制，因为 CLI 对控制台事件的优雅退出
// 在 taskkill 场景下收不到。
func killProcessTree(pid int) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
