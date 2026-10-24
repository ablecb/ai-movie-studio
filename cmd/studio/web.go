package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"aistudio/internal/web"
)

// cmdServe 启动 Web 控制台：浏览器里按业务模块操作整条流水线。
//
// Web 层不重新实现流水线——所有付费/重活都作为受控子进程调用本可执行文件的
// 各子命令（run/render/deliver/...），运行锁、断点续跑、预算熔断原样生效。
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "监听地址（仅本机访问；需要局域网访问时改为 0.0.0.0:8787）")
	open := fs.Bool("open", true, "启动后自动打开浏览器")
	fs.Parse(args)

	root, err := os.Getwd()
	if err != nil {
		return err
	}
	srv, err := web.NewServer(root)
	if err != nil {
		return err
	}
	hs := &http.Server{Addr: *addr, Handler: srv.Handler()}

	// Ctrl-C 优雅停机：等待中的请求收尾，运行中的任务进程随之退出
	go func() {
		<-rootCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}()

	url := "http://" + *addr
	fmt.Println("┌─────────────────────────────────────────────┐")
	fmt.Println("│   🎬 AI 电影工作室 · Web 控制台               │")
	fmt.Printf("│   %s\n", url)
	fmt.Println("│   业务模块：总览/创作/渲染/声音/交付/质检/运行/系统 │")
	fmt.Println("└─────────────────────────────────────────────┘")
	fmt.Printf("  工作区: %s\n", root)
	fmt.Println("  Ctrl-C 停止服务")
	if *open {
		go openBrowser(url)
	}
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	fmt.Println("\n服务已停止")
	return nil
}

// openBrowser 尽力打开默认浏览器，失败不影响服务。
func openBrowser(url string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		_ = exec.Command("open", url).Start()
	default:
		_ = exec.Command("xdg-open", url).Start()
	}
}
