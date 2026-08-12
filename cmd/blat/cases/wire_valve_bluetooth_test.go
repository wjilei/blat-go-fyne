package cases

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"blat/internal/core"
)

// fakeUI 满足 core.UI 接口，断言不会 panic。每个方法都用互斥锁保护，
// 因为 Confirm 等可能在不同 goroutine 被调用（运行时 PlanRunner 也有
// 并行选项，保守起见加锁）。
type fakeUI struct {
	mu sync.Mutex

	confirmRet bool
	confirmErr error
	confirmMsg string
	confirmN   int
}

func (f *fakeUI) Info(category, msg string) {}

func (f *fakeUI) Prompt(ctx context.Context, label, def string) (string, error) {
	return def, nil
}

func (f *fakeUI) WaitContinue(ctx context.Context, msg string) error { return nil }

func (f *fakeUI) Message(ctx context.Context, msg string, danger bool) error { return nil }

func (f *fakeUI) Confirm(ctx context.Context, msg string, danger bool) (bool, error) {
	f.mu.Lock()
	f.confirmMsg = msg
	f.confirmN++
	f.mu.Unlock()
	return f.confirmRet, f.confirmErr
}

func newObserveEnv(ui *fakeUI) *core.Env {
	return &core.Env{
		Ctx:  context.Background(),
		UI:   ui,
		Log:  &fakeLog{},
		Vars: map[string]any{},
		Devs: map[string]any{},
	}
}

// fakeLog 满足 core.Logger（Phase 2 A：三方法带 category 参数，测试中
// 丢弃 category 与消息内容）。
type fakeLog struct{}

func (fakeLog) Info(category, msg string)  {}
func (fakeLog) Warn(category, msg string)  {}
func (fakeLog) Error(category, msg string) {}

// 用户选「是」→ Run 应正常返回，且 Confirm 弹框文案应明确告诉用户
// 选否会失败（操作员可据此决策）。
func TestWireValveObserveValveCase_Yes(t *testing.T) {
	ui := &fakeUI{confirmRet: true}
	c := &WireValveObserveValveCase{}
	if err := c.Run(context.Background(), newObserveEnv(ui)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.confirmN != 1 {
		t.Errorf("Confirm 被调用 %d 次, 期望 1", ui.confirmN)
	}
	if !strings.Contains(ui.confirmMsg, "阀门") {
		t.Errorf("Confirm 文案 = %q, 期望提到「阀门」", ui.confirmMsg)
	}
	if !strings.Contains(ui.confirmMsg, "否") {
		t.Errorf("Confirm 文案 = %q, 期望明确告知选「否」会失败", ui.confirmMsg)
	}
}

// 用户选「否」→ Run 应返回非 nil 错误，case 失败。
func TestWireValveObserveValveCase_No(t *testing.T) {
	ui := &fakeUI{confirmRet: false}
	c := &WireValveObserveValveCase{}
	err := c.Run(context.Background(), newObserveEnv(ui))
	if err == nil {
		t.Fatal("期望返回错误, 实际为 nil")
	}
	if !strings.Contains(err.Error(), "未转动") && !strings.Contains(err.Error(), "否") {
		t.Errorf("错误信息 = %q, 期望提示用户选否/未转动", err.Error())
	}
}

// ctx 取消（Stop 按钮 / 关窗）→ Run 应返回 ctx.Err()。
func TestWireValveObserveValveCase_CtxCanceled(t *testing.T) {
	ui := &fakeUI{confirmErr: context.Canceled}
	c := &WireValveObserveValveCase{}
	err := c.Run(context.Background(), newObserveEnv(ui))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, 期望 errors.Is(..., context.Canceled)", err)
	}
}
