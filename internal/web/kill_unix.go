//go:build !windows

package web

import (
	"os/exec"
	"syscall"
	"time"
)

// applySysProcAttr Unix 下让子进程独立进程组，取消时整组终止
// （studio 子进程还会拉起 ffmpeg/ffprobe，只杀直接进程会留尾巴）。
func applySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree 先整组 SIGTERM（给 CLI 落盘断点的机会），3 秒后 SIGKILL。
func killProcessTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	go func() {
		time.Sleep(3 * time.Second)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}()
}
