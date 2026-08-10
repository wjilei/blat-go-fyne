package cases

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// askValveTurned 弹一个「是/否」确认框，让操作员确认电机校准后阀门已转动。
// 选「是」→ 返回 nil 继续；选「否」→ 返回 error（用例失败）；
// ctx 取消（Stop 按钮 / 关窗）→ 返回 ctx.Err()。回车默认「是」（参见
// internal/ui/fyne/app.go yesNoCh 处理，默认焦点在「是」上）。
func TestAskValveTurned_Yes(t *testing.T) {
	ui := &fakeUI{confirmRet: true}
	if err := askValveTurned(context.Background(), newObserveEnv(ui)); err != nil {
		t.Fatalf("选「是」应返回 nil, 实际: %v", err)
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.confirmN != 1 {
		t.Errorf("Confirm 调用次数 = %d, 期望 1（验证已合并为单次弹框）", ui.confirmN)
	}
	if !strings.Contains(ui.confirmMsg, "阀门") {
		t.Errorf("Confirm 文案 = %q, 期望提到「阀门」", ui.confirmMsg)
	}
}

// 选「否」→ Run 应返回非 nil 错误，便于调用方直接把错误冒泡出去。
func TestAskValveTurned_No(t *testing.T) {
	ui := &fakeUI{confirmRet: false}
	err := askValveTurned(context.Background(), newObserveEnv(ui))
	if err == nil {
		t.Fatal("选「否」应返回错误, 实际为 nil")
	}
	if !strings.Contains(err.Error(), "未转动") && !strings.Contains(err.Error(), "否") {
		t.Errorf("错误信息 = %q, 期望提示未转动/选否", err.Error())
	}
}

// ctx 取消（Stop 按钮 / 关窗）→ Run 应返回 ctx.Err()，让 PlanRunner 正确停止。
func TestAskValveTurned_CtxCanceled(t *testing.T) {
	ui := &fakeUI{confirmErr: context.Canceled}
	err := askValveTurned(context.Background(), newObserveEnv(ui))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, 期望 errors.Is(..., context.Canceled)", err)
	}
}
