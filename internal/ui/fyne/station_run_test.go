package fyneui

import (
	"context"
	"errors"
	"testing"
	"time"

	"blat/internal/report"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// ---------- 状态机 ----------

func TestStationStateString(t *testing.T) {
	cases := []struct {
		state stationState
		want  string
	}{
		{stIdle, "idle"},
		{stRunning, "running"},
		{stDone, "done"},
		{stFail, "fail"},
		{stStopped, "stopped"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestStationStateColorName(t *testing.T) {
	cases := []struct {
		state stationState
		want  fyne.ThemeColorName
	}{
		{stIdle, theme.ColorNameDisabled},
		{stRunning, theme.ColorNameWarning},
		{stDone, theme.ColorNameSuccess},
		{stFail, theme.ColorNameError},
		{stStopped, theme.ColorNameError},
	}
	for _, tc := range cases {
		if got := tc.state.ColorName(); got != tc.want {
			t.Errorf("%v.ColorName() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestStationRunStateMachine(t *testing.T) {
	r := &stationRun{}
	if got := r.State(); got != stIdle {
		t.Fatalf("initial state = %v, want idle", got)
	}
	r.SetState(stRunning, report.Summary{TotalNum: 2})
	if got := r.State(); got != stRunning {
		t.Fatalf("after SetState(running) = %v, want running", got)
	}
	r.SetState(stDone, report.Summary{TotalNum: 2, OKNum: 2, Result: 1})
	if got := r.State(); got != stDone {
		t.Fatalf("after SetState(done) = %v, want done", got)
	}
}

// ---------- onState 回调 ----------

func TestStationRunOnStateCallback(t *testing.T) {
	type call struct {
		old, new stationState
		sum      report.Summary
	}
	var calls []call
	r := &stationRun{
		onState: func(old, new stationState, sum report.Summary) {
			calls = append(calls, call{old, new, sum})
		},
	}
	r.SetState(stRunning, report.Summary{TotalNum: 2})
	r.SetState(stDone, report.Summary{TotalNum: 2, OKNum: 2, Result: 1})

	if len(calls) != 2 {
		t.Fatalf("onState called %d times, want 2", len(calls))
	}
	if calls[0].old != stIdle || calls[0].new != stRunning {
		t.Errorf("call[0] = %v->%v, want idle->running", calls[0].old, calls[0].new)
	}
	if calls[0].sum.TotalNum != 2 {
		t.Errorf("call[0].sum.TotalNum = %d, want 2", calls[0].sum.TotalNum)
	}
	if calls[1].old != stRunning || calls[1].new != stDone {
		t.Errorf("call[1] = %v->%v, want running->done", calls[1].old, calls[1].new)
	}
	if calls[1].sum.OKNum != 2 {
		t.Errorf("call[1].sum.OKNum = %d, want 2", calls[1].sum.OKNum)
	}

	// 相同状态不是迁移，不触发回调
	r.SetState(stDone, report.Summary{})
	if len(calls) != 2 {
		t.Errorf("same-state SetState triggered callback, calls = %d, want 2", len(calls))
	}
}

// ---------- deepCopyVars ----------

func TestDeepCopyVarsNested(t *testing.T) {
	orig := map[string]any{
		"HeatNote": map[string]any{
			"serial":    "123456",
			"mbus":      map[string]any{"port": "COM9"},
			"mbus_dev":  &struct{ name string }{name: "dev"},
			"bluetooth": &struct{ name string }{name: "bt"},
		},
		"top": []any{"a", map[string]any{"k": "v"}},
	}
	cp := deepCopyVars(orig)

	hn := cp["HeatNote"].(map[string]any)
	// 修改拷贝不影响原
	hn["serial"] = "changed"
	hn["mbus"].(map[string]any)["port"] = "COM10"
	cp["top"].([]any)[1].(map[string]any)["k"] = "changed"

	if orig["HeatNote"].(map[string]any)["serial"] != "123456" {
		t.Error("嵌套 map 修改泄漏到原 vars")
	}
	if orig["HeatNote"].(map[string]any)["mbus"].(map[string]any)["port"] != "COM9" {
		t.Error("嵌套 map 修改泄漏到原 vars")
	}
	if orig["top"].([]any)[1].(map[string]any)["k"] != "v" {
		t.Error("slice 修改泄漏到原 vars")
	}

	// 拷贝结果已删除 mbus_dev / bluetooth
	if _, ok := hn["mbus_dev"]; ok {
		t.Error("拷贝结果应删除 mbus_dev")
	}
	if _, ok := hn["bluetooth"]; ok {
		t.Error("拷贝结果应删除 bluetooth")
	}
	// 原 vars 不受影响
	if _, ok := orig["HeatNote"].(map[string]any)["mbus_dev"]; !ok {
		t.Error("原 vars 的 mbus_dev 不应被删除")
	}
	if _, ok := orig["HeatNote"].(map[string]any)["bluetooth"]; !ok {
		t.Error("原 vars 的 bluetooth 不应被删除")
	}
}

func TestDeepCopyVarsNoHeatNote(t *testing.T) {
	orig := map[string]any{"a": 1, "b": "x", "c": []string{"s1", "s2"}}
	cp := deepCopyVars(orig)
	cp["a"] = 2
	cp["c"].([]string)[0] = "changed"
	if orig["a"] != 1 {
		t.Error("顶层修改泄漏到原 vars")
	}
	if orig["c"].([]string)[0] != "s1" {
		t.Error("[]string slice 修改泄漏到原 vars")
	}
}

func TestDeepCopyVarsPointerShared(t *testing.T) {
	type dev struct{ name string }
	d := &dev{name: "dev"}
	orig := map[string]any{"dev": d}
	cp := deepCopyVars(orig)
	if cp["dev"] != d {
		t.Error("指针应原样引用（不深拷贝）")
	}
}

func TestDeepCopyVarsNil(t *testing.T) {
	if got := deepCopyVars(nil); got != nil {
		t.Errorf("deepCopyVars(nil) = %v, want nil", got)
	}
}

// ---------- stationUI ----------

// fakeUI 记录 core.UI 调用参数，供 stationUI 前缀/透传测试使用。
type fakeUI struct {
	infoCat, infoMsg string

	promptCtx   context.Context
	promptLabel string
	promptDef   string
	promptOut   string
	promptErr   error

	waitCtx context.Context
	waitMsg string
	waitErr error

	msgCtx    context.Context
	msgMsg    string
	msgDanger bool
	msgErr    error

	confirmCtx    context.Context
	confirmMsg    string
	confirmDanger bool
	confirmOut    bool
	confirmErr    error
}

func (f *fakeUI) Info(category, msg string) {
	f.infoCat, f.infoMsg = category, msg
}

func (f *fakeUI) Prompt(ctx context.Context, label, def string) (string, error) {
	f.promptCtx, f.promptLabel, f.promptDef = ctx, label, def
	return f.promptOut, f.promptErr
}

func (f *fakeUI) WaitContinue(ctx context.Context, msg string) error {
	f.waitCtx, f.waitMsg = ctx, msg
	return f.waitErr
}

func (f *fakeUI) Message(ctx context.Context, msg string, danger bool) error {
	f.msgCtx, f.msgMsg, f.msgDanger = ctx, msg, danger
	return f.msgErr
}

func (f *fakeUI) Confirm(ctx context.Context, msg string, danger bool) (bool, error) {
	f.confirmCtx, f.confirmMsg, f.confirmDanger = ctx, msg, danger
	return f.confirmOut, f.confirmErr
}

func TestStationUIPrefixAndPassthrough(t *testing.T) {
	base := &fakeUI{}
	u := newStationUI(base, "设备1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Prompt：label 加前缀，def/返回值/ctx 透传
	base.promptOut, base.promptErr = "输入值", nil
	got, err := u.Prompt(ctx, "请输入序列号", "默认值")
	if err != nil || got != "输入值" {
		t.Fatalf("Prompt = (%q, %v), want (输入值, nil)", got, err)
	}
	if base.promptLabel != "【设备1】请输入序列号" {
		t.Errorf("Prompt label = %q, want %q", base.promptLabel, "【设备1】请输入序列号")
	}
	if base.promptDef != "默认值" {
		t.Errorf("Prompt def = %q, want 默认值", base.promptDef)
	}
	if base.promptCtx != ctx {
		t.Error("Prompt ctx 未透传")
	}

	// WaitContinue：msg 加前缀，错误/ctx 透传
	if err := u.WaitContinue(ctx, "请确认"); err != nil {
		t.Fatalf("WaitContinue err = %v, want nil", err)
	}
	if base.waitMsg != "【设备1】请确认" {
		t.Errorf("WaitContinue msg = %q, want %q", base.waitMsg, "【设备1】请确认")
	}
	if base.waitCtx != ctx {
		t.Error("WaitContinue ctx 未透传")
	}

	// Message：msg 加前缀，danger/错误/ctx 透传
	if err := u.Message(ctx, "测试完成", true); err != nil {
		t.Fatalf("Message err = %v, want nil", err)
	}
	if base.msgMsg != "【设备1】测试完成" {
		t.Errorf("Message msg = %q, want %q", base.msgMsg, "【设备1】测试完成")
	}
	if !base.msgDanger {
		t.Error("Message danger 未透传")
	}
	if base.msgCtx != ctx {
		t.Error("Message ctx 未透传")
	}

	// Confirm：msg 加前缀，danger/返回值/错误/ctx 透传
	base.confirmOut, base.confirmErr = true, nil
	ok, err := u.Confirm(ctx, "是否继续", false)
	if err != nil || !ok {
		t.Fatalf("Confirm = (%v, %v), want (true, nil)", ok, err)
	}
	if base.confirmMsg != "【设备1】是否继续" {
		t.Errorf("Confirm msg = %q, want %q", base.confirmMsg, "【设备1】是否继续")
	}
	if base.confirmDanger {
		t.Error("Confirm danger 未透传")
	}
	if base.confirmCtx != ctx {
		t.Error("Confirm ctx 未透传")
	}

	// Info：日志透传不加前缀
	u.Info("RUNNER", "case_start xxx")
	if base.infoCat != "RUNNER" || base.infoMsg != "case_start xxx" {
		t.Errorf("Info = (%q, %q), want (RUNNER, case_start xxx)", base.infoCat, base.infoMsg)
	}
}

func TestStationUIErrorPassthrough(t *testing.T) {
	base := &fakeUI{}
	u := newStationUI(base, "设备2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	boom := errors.New("boom")
	base.promptErr = boom
	if _, err := u.Prompt(ctx, "x", ""); err != boom {
		t.Errorf("Prompt err = %v, want boom", err)
	}
	base.waitErr = boom
	if err := u.WaitContinue(ctx, "x"); err != boom {
		t.Errorf("WaitContinue err = %v, want boom", err)
	}
	base.msgErr = boom
	if err := u.Message(ctx, "x", false); err != boom {
		t.Errorf("Message err = %v, want boom", err)
	}
	base.confirmErr = boom
	if _, err := u.Confirm(ctx, "x", false); err != boom {
		t.Errorf("Confirm err = %v, want boom", err)
	}
}

func TestStationUICtxCancelPropagates(t *testing.T) {
	base := &fakeUI{}
	u := newStationUI(base, "设备1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// ctx 已取消：透传同一个 ctx，base 应能观察到 ctx.Err()==context.Canceled
	base.promptErr = ctx.Err()
	if _, err := u.Prompt(ctx, "x", ""); err != context.Canceled {
		t.Errorf("Prompt err = %v, want context.Canceled", err)
	}
	if base.promptCtx != ctx {
		t.Error("Prompt ctx 未透传")
	}
}

// ---------- stationAdapter ----------

func TestStationAdapterLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &stationRun{ctx: ctx}
	ad := &stationAdapter{run: r}

	// OnPlanStart → running，summary 记录 TotalNum
	ad.OnPlanStart(3, time.Now())
	if r.State() != stRunning {
		t.Fatalf("after OnPlanStart state = %v, want running", r.State())
	}
	if r.summary.TotalNum != 3 {
		t.Errorf("summary.TotalNum = %d, want 3", r.summary.TotalNum)
	}

	// OnCaseStart/OnCaseStop 不改变状态
	ad.OnCaseStart(1, report.CaseReport{Seq: 1, Result: report.CaseRunning})
	ad.OnCaseStop(1, report.CaseReport{Seq: 1, Result: report.CaseOK})
	if r.State() != stRunning {
		t.Fatalf("after OnCaseStart/Stop state = %v, want running", r.State())
	}

	// 成功 OnPlanStop → done，summary 更新
	ad.OnPlanStop(report.Summary{TotalNum: 3, OKNum: 3, Result: 1, Reason: "ok"})
	if r.State() != stDone {
		t.Fatalf("success OnPlanStop state = %v, want done", r.State())
	}
	if r.summary.OKNum != 3 || r.summary.Result != 1 {
		t.Errorf("summary = %+v, want OKNum=3 Result=1", r.summary)
	}
}

func TestStationAdapterFail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &stationRun{ctx: ctx}
	ad := &stationAdapter{run: r}
	ad.OnPlanStart(2, time.Now())
	ad.OnPlanStop(report.Summary{TotalNum: 2, OKNum: 1, FailNum: 1, Result: 0, Reason: "case x failed"})
	if r.State() != stFail {
		t.Fatalf("fail OnPlanStop state = %v, want fail", r.State())
	}
	if r.summary.FailNum != 1 {
		t.Errorf("summary.FailNum = %d, want 1", r.summary.FailNum)
	}
}

func TestStationAdapterStoppedOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &stationRun{ctx: ctx}
	ad := &stationAdapter{run: r}
	ad.OnPlanStart(2, time.Now())
	cancel()
	ad.OnPlanStop(report.Summary{TotalNum: 2, OKNum: 1, FailNum: 0, Result: 1})
	if r.State() != stStopped {
		t.Fatalf("cancelled OnPlanStop state = %v, want stopped", r.State())
	}
}