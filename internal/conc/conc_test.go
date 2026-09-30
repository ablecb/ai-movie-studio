package conc

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestForEachIdxRunsEveryIndexExactlyOnce 每个索引必须恰好执行一次——
// 这是"结果按索引写回预分配切片"这一并发约定的根基。
func TestForEachIdxRunsEveryIndexExactlyOnce(t *testing.T) {
	n := 100
	counts := make([]int32, n)
	ForEachIdx(n, 8, func(i int) {
		atomic.AddInt32(&counts[i], 1)
	})
	for i, c := range counts {
		if c != 1 {
			t.Fatalf("索引 %d 执行了 %d 次，期望恰好 1 次", i, c)
		}
	}
}

// TestForEachIdxBoundedConcurrency 并发度必须有界：同时在跑的 goroutine
// 不得超过 workers（原语用信号量派发，超额说明实现错了）。
func TestForEachIdxBoundedConcurrency(t *testing.T) {
	var cur, peak int32
	var mu sync.Mutex
	ForEachIdx(50, 4, func(i int) {
		c := atomic.AddInt32(&cur, 1)
		mu.Lock()
		if c > peak {
			peak = c
		}
		mu.Unlock()
		atomic.AddInt32(&cur, -1)
	})
	if peak > 4 {
		t.Fatalf("峰值并发 %d 超过上限 4", peak)
	}
}

// TestForEachIdxSmallBatch workers 大于任务数时不应起多余 goroutine；
// n<=0 必须直接返回（不 panic）。
func TestForEachIdxSmallBatch(t *testing.T) {
	var n atomic.Int32
	ForEachIdx(2, 100, func(i int) { n.Add(1) })
	if n.Load() != 2 {
		t.Fatalf("执行了 %d 次，期望 2 次", n.Load())
	}
	ForEachIdx(0, 4, func(i int) { t.Fatal("n=0 不应执行任何任务") })
	ForEachIdx(-3, 4, func(i int) { t.Fatal("n<0 不应执行任何任务") })
}
