// Package conc 提供项目内共用的有界并发原语。
//
// 渲染管线（render.generateAll）自带 worker 池；这里服务其余"逐条串行、
// 彼此独立"的环节（TTS 逐句、参考帧逐角色、视觉复核逐批、时长探测逐文件）。
// 只负责"有界并发地跑完 n 个任务"：结果由调用方按索引写回预分配的切片，
// 错误与降级语义也由调用方决定——每个环节的失败策略本来就不同
// （首错中止 / 单项失败不放弃整轮 / 单项失败忽略）。
package conc

import "sync"

// ForEachIdx 以最多 workers 个 goroutine 并发执行 fn(i)，i ∈ [0,n)。
//
// 并发约定：
//   - fn 只写"自己的"槽位（预分配切片的 [i] 下标）或只读共享输入；
//     需要累计的共享状态由调用方自行加锁
//   - workers<=0 取 4；大于 n 取 n（不为小批量起多余 goroutine）
//   - 不 recover：与 render.generateAll 的并发风格一致，panic 直接崩溃暴露问题
//
// 派发循环在信号量上阻塞，因此 fn 开头检查"失败标志"即可实现
// 出错后不再派发新任务（正在执行的任务自然跑完）。
func ForEachIdx(n, workers int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if workers <= 0 {
		workers = 4
	}
	if workers > n {
		workers = n
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
