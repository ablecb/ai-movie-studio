package ai

import (
	"context"
	"testing"
)

// 预算预留回归：并发 worker 提交付费任务与实际过账之间存在窗口，
// 没有在途预留时 N 个 worker 可以同时过闸击穿预算（check-then-act 超卖）。

func TestReserveBlocksWhenEstimateExceedsBudget(t *testing.T) {
	c := New(Config{Mock: true})
	c.SetBudget(10)

	if err := c.reserve("videos.generations", 9); err != nil {
		t.Fatalf("预算内预留应成功: %v", err)
	}
	// 在途 9 + 新请求 2 > 预算 10 → 必须拒绝
	err := c.reserve("videos.generations", 2)
	if err == nil {
		t.Fatal("在途预留吃满预算后新调用应被拒绝")
	}
	if !IsNoFund(err) {
		t.Fatalf("应分类为余额不足（no_fund），实际: %v", err)
	}
	// 结算释放后在途归零，新调用恢复可用
	c.settle(9)
	if err := c.reserve("videos.generations", 2); err != nil {
		t.Fatalf("结算释放后应可继续预留: %v", err)
	}
}

func TestSettleNeverDrivesInflightNegative(t *testing.T) {
	c := New(Config{Mock: true})
	c.SetBudget(10)
	if err := c.reserve("images.generations", 3); err != nil {
		t.Fatal(err)
	}
	c.settle(5) // 故意多结算（防御性口径）
	if c.inflight() != 0 {
		t.Fatalf("在途预留不允许为负，实际 %.2f", c.inflight())
	}
}

func TestGuardBudgetIncludesInflight(t *testing.T) {
	c := New(Config{Mock: true})
	c.SetBudget(10)
	if err := c.reserve("videos.generations", 10); err != nil {
		t.Fatal(err)
	}
	// 预算被在途完全占用：零预估的门禁调用也必须被挡住
	err := c.guardBudget("chat:test")
	if err == nil {
		t.Fatal("在途吃满预算后零预估调用应被拒绝")
	}
	if !IsNoFund(err) {
		t.Fatalf("错误分类应为 no_fund: %v", err)
	}
}

func TestWarnFiresOnceAcrossClients(t *testing.T) {
	c := New(Config{Mock: true})
	c.SetBudget(10)
	c.SetWarnRatio(0.8)
	if err := c.reserve("x", 8); err != nil { // 触发 80% 告警
		t.Fatal(err)
	}
	c.settle(8)
	// warned 标志在共享状态里去重：第二次到达告警线不再重复（此处验证不 panic 且可继续）
	if err := c.reserve("x", 1); err != nil {
		t.Fatal(err)
	}
}

func TestSharedStateAcrossDerivedClients(t *testing.T) {
	// For() 派生客户端与母客户端必须共享同一份可变状态：
	// 任一客户端的预留/结算对全部客户端可见
	base := New(Config{Mock: true})
	base.SetBudget(10)
	derived := base.For(CapVision, Endpoint{})

	if err := base.reserve("vision", 10); err != nil {
		t.Fatal(err)
	}
	if err := derived.guardBudget("chat:x"); err == nil {
		t.Fatal("派生客户端必须看到母客户端的在途预留（否则预算门禁形同虚设）")
	}
	if derived.Context() != base.Context() {
		t.Fatal("派生客户端必须共享批次上下文（取消要覆盖全部付费出口）")
	}
	// 派生客户端的 SetParentContext 对母客户端同样生效（共享状态）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	derived.SetParentContext(ctx)
	if base.Context() != ctx {
		t.Fatal("共享状态下派生客户端设置批次上下文应全局生效")
	}
}
