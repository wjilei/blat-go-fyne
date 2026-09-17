package cases

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"blat/internal/core"
	"blat/internal/device/mbus"
)

// ---- askValveTurning（弹框 1：用户确认电机开始转动） ----

// 选「是」→ 返回 nil；Confirm 文案必须带上刚设置的开度值和「电机」。
func TestAskValveTurning_Yes(t *testing.T) {
	ui := &fakeUI{confirmRet: true}
	if err := askValveTurning(context.Background(), newObserveEnv(ui), 80); err != nil {
		t.Fatalf("选「是」应返回 nil, 实际: %v", err)
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.confirmN != 1 {
		t.Errorf("Confirm 调用次数 = %d, 期望 1", ui.confirmN)
	}
	if !strings.Contains(ui.confirmMsg, "80") {
		t.Errorf("Confirm 文案 = %q, 期望提到开度 80", ui.confirmMsg)
	}
	if !strings.Contains(ui.confirmMsg, "电机") {
		t.Errorf("Confirm 文案 = %q, 期望提到「电机」", ui.confirmMsg)
	}
	if !strings.Contains(ui.confirmMsg, "否") {
		t.Errorf("Confirm 文案 = %q, 期望明确告知选「否」会失败", ui.confirmMsg)
	}
}

// 选「否」→ 应返回非 nil 错误。
func TestAskValveTurning_No(t *testing.T) {
	ui := &fakeUI{confirmRet: false}
	err := askValveTurning(context.Background(), newObserveEnv(ui), 80)
	if err == nil {
		t.Fatal("选「否」应返回错误, 实际为 nil")
	}
	if !strings.Contains(err.Error(), "未转动") && !strings.Contains(err.Error(), "否") {
		t.Errorf("错误信息 = %q, 期望提示未转动/选否", err.Error())
	}
}

// ctx 取消 → 应返回 ctx.Err()。
func TestAskValveTurning_CtxCanceled(t *testing.T) {
	ui := &fakeUI{confirmErr: context.Canceled}
	err := askValveTurning(context.Background(), newObserveEnv(ui), 80)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, 期望 errors.Is(..., context.Canceled)", err)
	}
}

// ---- askValveDoneWait（弹框 2：用户等电机转完点确认） ----

// 用户点确认 → 返回 nil；Message 文案必须带上当前开度、目标开度、
// 以及「确定」按钮提示词。
func TestAskValveDoneWait_Confirmed(t *testing.T) {
	ui := &fakeUI{messageErr: nil}
	if err := askValveDoneWait(context.Background(), newObserveEnv(ui), 80, 100); err != nil {
		t.Fatalf("点「确定」应返回 nil, 实际: %v", err)
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.messageN != 1 {
		t.Errorf("Message 调用次数 = %d, 期望 1", ui.messageN)
	}
	if !strings.Contains(ui.messageLast, "80") {
		t.Errorf("Message 文案 = %q, 期望提到当前开度 80", ui.messageLast)
	}
	if !strings.Contains(ui.messageLast, "100") {
		t.Errorf("Message 文案 = %q, 期望提到目标开度 100", ui.messageLast)
	}
	if !strings.Contains(ui.messageLast, "确定") {
		t.Errorf("Message 文案 = %q, 期望提到「确定」按钮", ui.messageLast)
	}
}

// Message 实现返回错误（如 ctx 取消 / 关窗）→ 应透传。
func TestAskValveDoneWait_CtxCanceled(t *testing.T) {
	ui := &fakeUI{messageErr: context.Canceled}
	err := askValveDoneWait(context.Background(), newObserveEnv(ui), 80, 100)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, 期望 errors.Is(..., context.Canceled)", err)
	}
}

// ---- Configure ----

// 默认参数：未传 args 时取 80/100。
func TestWireValveMBusReadMotorCase_Configure_Defaults(t *testing.T) {
	c := &WireValveMBusReadMotorCase{}
	if err := c.Configure(nil); err != nil {
		t.Fatalf("Configure(nil) 意外错误: %v", err)
	}
	if c.step1Open != 80 || c.step2Open != 100 {
		t.Errorf("默认参数 = %+v, 期望 80/100", c)
	}
}

// 中文键：第一阶段开度/第二阶段开度。
func TestWireValveMBusReadMotorCase_Configure_ChineseKeys(t *testing.T) {
	c := &WireValveMBusReadMotorCase{}
	args := map[string]any{
		"第一阶段开度": 50,
		"第二阶段开度": 90,
	}
	if err := c.Configure(args); err != nil {
		t.Fatalf("Configure 意外错误: %v", err)
	}
	if c.step1Open != 50 || c.step2Open != 90 {
		t.Errorf("中文键参数 = %+v, 期望 50/90", c)
	}
}

// 英文键：step1_open/step2_open。
func TestWireValveMBusReadMotorCase_Configure_EnglishKeys(t *testing.T) {
	c := &WireValveMBusReadMotorCase{}
	args := map[string]any{
		"step1_open": 30,
		"step2_open": 70,
	}
	if err := c.Configure(args); err != nil {
		t.Fatalf("Configure 意外错误: %v", err)
	}
	if c.step1Open != 30 || c.step2Open != 70 {
		t.Errorf("英文键参数 = %+v, 期望 30/70", c)
	}
}

// 中文键优先于英文键：同传时取中文。
func TestWireValveMBusReadMotorCase_Configure_ChinesePreferredOverEnglish(t *testing.T) {
	c := &WireValveMBusReadMotorCase{}
	args := map[string]any{
		"第一阶段开度":   50,
		"step1_open": 99, // 应被中文覆盖
	}
	if err := c.Configure(args); err != nil {
		t.Fatalf("Configure 意外错误: %v", err)
	}
	if c.step1Open != 50 {
		t.Errorf("step1Open = %d, 期望中文键 50 优先", c.step1Open)
	}
}

// 非法值（开度超 100 / 负数）忽略保留默认。
func TestWireValveMBusReadMotorCase_Configure_InvalidIgnored(t *testing.T) {
	c := &WireValveMBusReadMotorCase{}
	args := map[string]any{
		"第一阶段开度": 200, // 越界
		"第二阶段开度": -5,  // 越界
	}
	if err := c.Configure(args); err != nil {
		t.Fatalf("Configure 意外错误: %v", err)
	}
	if c.step1Open != 80 || c.step2Open != 100 {
		t.Errorf("越界开度应保留默认, 得到 step1=%d step2=%d", c.step1Open, c.step2Open)
	}
}

// YAML 解析出的 float64 也要被正确识别。
func TestWireValveMBusReadMotorCase_Configure_YAMLFloat(t *testing.T) {
	c := &WireValveMBusReadMotorCase{}
	args := map[string]any{
		"第一阶段开度": float64(45),
	}
	if err := c.Configure(args); err != nil {
		t.Fatalf("Configure 意外错误: %v", err)
	}
	if c.step1Open != 45 {
		t.Errorf("YAML float64 参数 = step1=%d, 期望 45", c.step1Open)
	}
}

// ---- Run ----

// newMBusRunEnv 构造一个能跑 WireValveMBusReadMotorCase.Run 的 mock 环境：
// mock mbus 设备直接通过 Vars.HeatNote["mbus_dev"] 注入（_ensureMBUS 命中后
// 直接 Connect），mbus_mock=true 标记实例为 mock；serial 从 heatnote 读取。
func newMBusRunEnv(ui *fakeUI, dev *mbus.Device) *core.Env {
	heatnote := map[string]any{
		"mbus_mock": true,
		"mbus":      map[string]any{"port": "COM9"},
		"serial":    "262601300011",
		"mbus_dev":  dev,
	}
	return &core.Env{
		Ctx:  context.Background(),
		UI:   ui,
		Log:  &fakeLog{},
		Vars: map[string]any{"HeatNote": heatnote},
		Devs: map[string]any{},
	}
}

// recordLog 记录 Info/Warn/Error 消息文本，用于断言关键步骤日志
// （区别于无输出的 fakeLog）。
type recordLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *recordLog) Info(category, msg string)  { l.add(msg) }
func (l *recordLog) Warn(category, msg string)  { l.add(msg) }
func (l *recordLog) Error(category, msg string) { l.add(msg) }

func (l *recordLog) add(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
}

func (l *recordLog) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// newMBusRunEnvLog 同 newMBusRunEnv，但日志换成指定 logger（便于断言
// MbusReadInfo 调用日志）。
func newMBusRunEnvLog(ui *fakeUI, dev *mbus.Device, log core.Logger) *core.Env {
	env := newMBusRunEnv(ui, dev)
	env.Log = log
	return env
}

// happy path：弹框 1 选是 + 弹框 2 点确定 → Run 成功，且 SetValveOpenpre
// 被调用两次（80 → 100），日志写入断电提醒。
func TestWireValveMBusReadMotorCase_Run_HappyPath(t *testing.T) {
	dev := mbus.NewMockDevice()
	if err := dev.Connect(context.Background(), "COM9"); err != nil {
		t.Fatalf("Connect 意外错误: %v", err)
	}
	ui := &fakeUI{confirmRet: true} // Confirm = 是
	c := &WireValveMBusReadMotorCase{}
	if err := c.Configure(nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := c.Run(context.Background(), newMBusRunEnv(ui, dev)); err != nil {
		t.Fatalf("Run 期望成功, 实际: %v", err)
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.confirmN != 1 {
		t.Errorf("Confirm(电机转动) 调用 %d, 期望 1", ui.confirmN)
	}
	if ui.messageN != 1 {
		t.Errorf("Message(等转完) 调用 %d, 期望 1", ui.messageN)
	}
	if !strings.Contains(ui.confirmMsg, "80") {
		t.Errorf("Confirm 文案 = %q, 期望提到阶段 1 开度 80", ui.confirmMsg)
	}
	if !strings.Contains(ui.messageLast, "80") || !strings.Contains(ui.messageLast, "100") {
		t.Errorf("Message 文案 = %q, 期望同时提到 80 和 100", ui.messageLast)
	}
}

// 弹框 1 选否 → Run 应立即失败（不应进入弹框 2）。
func TestWireValveMBusReadMotorCase_Run_UserSaysNoAtFirstConfirm(t *testing.T) {
	dev := mbus.NewMockDevice()
	if err := dev.Connect(context.Background(), "COM9"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ui := &fakeUI{confirmRet: false}
	c := &WireValveMBusReadMotorCase{}
	if err := c.Configure(nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	err := c.Run(context.Background(), newMBusRunEnv(ui, dev))
	if err == nil {
		t.Fatal("弹框 1 选否应使 Run 返回错误")
	}
	if !strings.Contains(err.Error(), "未转动") && !strings.Contains(err.Error(), "否") {
		t.Errorf("错误信息 = %q, 期望提示未转动/选否", err.Error())
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.messageN != 0 {
		t.Errorf("弹框 1 选否后不应进入弹框 2, Message 被调用 %d 次", ui.messageN)
	}
}

// ctx 在弹框 2 处取消 → Run 应返回 ctx.Err()。
func TestWireValveMBusReadMotorCase_Run_CtxCanceledAtMessage(t *testing.T) {
	dev := mbus.NewMockDevice()
	if err := dev.Connect(context.Background(), "COM9"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ui := &fakeUI{
		confirmRet: true,
		messageErr: context.Canceled,
	}
	c := &WireValveMBusReadMotorCase{}
	if err := c.Configure(nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	err := c.Run(context.Background(), newMBusRunEnv(ui, dev))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, 期望 errors.Is(..., context.Canceled)", err)
	}
}

// 阶段 1.6 alarm 校验（Alarm=0 正常）：MbusReadInfo 返回 Alarm=0 → 流程
// 正常继续到阶段 2 恢复开度，且日志记录无告警。
func TestWireValveMBusReadMotorCase_Run_ReadAlarmOK(t *testing.T) {
	dev := mbus.NewMockDevice()
	if err := dev.Connect(context.Background(), "COM9"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	dev.SetMockInfo(mbus.MbusInfo{Alarm: 0})
	ui := &fakeUI{confirmRet: true}
	log := &recordLog{}
	c := &WireValveMBusReadMotorCase{}
	if err := c.Configure(nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := c.Run(context.Background(), newMBusRunEnvLog(ui, dev, log)); err != nil {
		t.Fatalf("Alarm=0 应继续流程成功, 实际: %v", err)
	}
	ui.mu.Lock()
	if ui.messageN != 1 {
		t.Errorf("阶段 2 应继续执行, Message(等转完) 调用 %d, 期望 1", ui.messageN)
	}
	ui.mu.Unlock()
	if !log.contains("无告警") {
		t.Errorf("日志应记录无告警信息, 实际: %v", log.msgs)
	}
}

// Alarm 非 0 失败：MbusReadInfo 返回 Alarm=1 → Run 应失败，错误信息包含
// 告警位实际值，且流程在阶段 2 恢复开度之前中断。
func TestWireValveMBusReadMotorCase_Run_ReadAlarmNonZero(t *testing.T) {
	dev := mbus.NewMockDevice()
	if err := dev.Connect(context.Background(), "COM9"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	dev.SetMockInfo(mbus.MbusInfo{Alarm: 1})
	ui := &fakeUI{confirmRet: true}
	c := &WireValveMBusReadMotorCase{}
	if err := c.Configure(nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	err := c.Run(context.Background(), newMBusRunEnv(ui, dev))
	if err == nil {
		t.Fatal("Alarm 非 0 应使 Run 返回错误")
	}
	if !strings.Contains(err.Error(), "告警") || !strings.Contains(err.Error(), "1") {
		t.Errorf("错误信息 = %q, 期望提到「告警」与实际值 1", err.Error())
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.messageN != 1 {
		t.Errorf("Alarm 校验失败应在阶段 2 前中断, Message 调用 %d, 期望 1（仅等转完确认框）", ui.messageN)
	}
}