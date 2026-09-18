package fyneui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/device/mbus"
	"blat/internal/runtime"

	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// newPanelTestApp 构造一个不依赖真实窗口的 App 骨架：
//   - 三工位槽位（stations/stationRuns/stationMBUS）为空数组
//   - logf 为 nil：appendLog 的 fyne.Do 走 test 驱动内联执行，refreshLog
//     因 logf==nil 直接返回，不会碰 UI
//   - shutdown 已关闭：switchMode/onPlanSelected 拒绝路径里的 a.Message
//     立即以"ui shutdown"返回，不阻塞测试
func newPanelTestApp(t *testing.T) *App {
	t.Helper()
	ta := test.NewApp()
	t.Cleanup(ta.Quit)
	a := &App{
		mu:                  sync.Mutex{},
		env:                 &core.Env{Vars: map[string]any{}},
		stations:            make([]*stationPanel, 3),
		stationRuns:         make([]*stationRun, 3),
		stationMBUS:         make([]*stationMBUS, 3),
		messageCh:           make(chan messageReq, 1),
		shutdown:            make(chan struct{}),
		stationShutdownDone: make(chan struct{}),
	}
	close(a.shutdown)
	return a
}

// ---------- stationsBusy ----------

func TestStationsBusy(t *testing.T) {
	a := newPanelTestApp(t)
	if a.stationsBusy() {
		t.Fatal("三槽位全空时不应 busy")
	}
	// 占位 boot 阶段（槽位非 nil 但还没真 run）也应算 busy
	a.stationRuns[1] = &stationRun{sn: "123456789012"}
	if !a.stationsBusy() {
		t.Fatal("任一槽位非 nil（占位/运行中）应 busy")
	}
}

// ---------- applyConfig busy guard ----------

func TestApplyConfigRejectsWhileStationBusy(t *testing.T) {
	a := newPanelTestApp(t)
	a.varsFile = filepath.Join(t.TempDir(), "env.yml")
	a.env.Vars["HeatNote"] = map[string]any{"mbus": map[string]any{"port": "COM5"}}
	a.stationRuns[0] = &stationRun{sn: "123456789012"} // 工位正在运行

	a.applyConfig("COM9", `C:\fw\a.bin`)

	mbusCfg := a.env.Vars["HeatNote"].(map[string]any)["mbus"].(map[string]any)
	if mbusCfg["port"] != "COM5" {
		t.Errorf("busy 时保存应被拒绝：port = %v, want COM5（保持原值）", mbusCfg["port"])
	}
	if _, ok := mbusCfg["firmware"]; ok {
		t.Error("busy 时保存应被拒绝：不应写入 firmware")
	}
	if _, err := os.Stat(a.varsFile); !os.IsNotExist(err) {
		t.Errorf("busy 时不应落盘，varsFile 不应存在（err=%v）", err)
	}
}

func TestApplyConfigSavesWhenIdle(t *testing.T) {
	a := newPanelTestApp(t)
	a.varsFile = filepath.Join(t.TempDir(), "env.yml")
	a.env.Vars["HeatNote"] = map[string]any{"mbus": map[string]any{"port": "COM5"}}

	a.applyConfig("COM9", `C:\fw\a.bin`)

	mbusCfg := a.env.Vars["HeatNote"].(map[string]any)["mbus"].(map[string]any)
	if mbusCfg["port"] != "COM9" {
		t.Errorf("port = %v, want COM9", mbusCfg["port"])
	}
	if mbusCfg["firmware"] != `C:\fw\a.bin` {
		t.Errorf("firmware = %v, want C:\\fw\\a.bin", mbusCfg["firmware"])
	}
	data, err := os.ReadFile(a.varsFile)
	if err != nil {
		t.Fatalf("读取落盘 env.yml 失败: %v", err)
	}
	if !strings.Contains(string(data), "firmware") {
		t.Errorf("落盘 env.yml 应包含 firmware 键:\n%s", data)
	}
}

func TestApplyConfigEmptyFirmwareDeletesKey(t *testing.T) {
	a := newPanelTestApp(t)
	a.varsFile = filepath.Join(t.TempDir(), "env.yml")
	a.env.Vars["HeatNote"] = map[string]any{
		"mbus": map[string]any{"port": "COM5", "firmware": `C:\fw\old.bin`},
	}

	a.applyConfig("COM9", "")

	mbusCfg := a.env.Vars["HeatNote"].(map[string]any)["mbus"].(map[string]any)
	if _, ok := mbusCfg["firmware"]; ok {
		t.Error("清空升级文件后内存不应保留 firmware 键")
	}
	data, err := os.ReadFile(a.varsFile)
	if err != nil {
		t.Fatalf("读取落盘 env.yml 失败: %v", err)
	}
	if strings.Contains(string(data), "firmware") {
		t.Errorf("清空升级文件后落盘 env.yml 不应含 firmware 键:\n%s", data)
	}
}

// ---------- plan 切换拒绝 ----------

func TestSwitchModeRejectsWhileStationBusy(t *testing.T) {
	a := newPanelTestApp(t)
	a.mode = "panel"

	// panel→panel 且工位运行中：拒绝（不覆盖 a.plan）
	a.stationRuns[0] = &stationRun{sn: "123456789012"}
	if a.switchMode(true) {
		t.Error("panel→panel 且工位运行中应拒绝切换")
	}
	a.stationRuns[0] = nil
	if !a.switchMode(true) {
		t.Error("panel→panel 空闲时应允许切换")
	}

	// single→panel 且工位运行中：拒绝
	a.mode = "single"
	a.stationRuns[0] = &stationRun{sn: "123456789012"}
	if a.switchMode(true) {
		t.Error("single→panel 且工位运行中应拒绝切换")
	}

	// panel→single 且工位运行中：拒绝（原有行为回归）
	a.mode = "panel"
	a.stationRuns[0] = &stationRun{sn: "123456789012"}
	if a.switchMode(false) {
		t.Error("panel→single 且工位运行中应拒绝切换")
	}
	a.stationRuns[0] = nil
	if !a.switchMode(false) {
		t.Error("panel→single 空闲时应允许切换")
	}
}

func TestOnPlanSelectedRejectsWhileStationBusy(t *testing.T) {
	a := newPanelTestApp(t)
	a.planItems = []PlanItem{{Name: "PTVB1 整机测试", Path: "confs/plan_PTVB1_ut_check_state.yml"}}
	a.planSel = widget.NewSelect([]string{planPlaceholder, "PTVB1 整机测试"}, nil)
	a.mode = "panel"
	a.stationRuns[0] = &stationRun{sn: "123456789012"}

	a.onPlanSelected("PTVB1 整机测试")

	if a.plan != nil {
		t.Error("工位运行中不应加载新 plan（不覆盖 a.plan）")
	}
	if a.planSel.Selected != planPlaceholder {
		t.Errorf("拒绝后下拉框应回退到 %q, got %q", planPlaceholder, a.planSel.Selected)
	}
}

// ---------- stopAllStations 设备生命周期 ----------

// TestStopAllStationsDeviceLifecycle 验证关窗收尾不会在活跃 run 事务期间
// 提前 Disconnect 正在使用的缓存设备：
//   - 空闲缓存设备（inUse=false）由 stopAllStations 立即断开；
//   - in-use 设备（被活跃 run 复用）由 run 自己的收尾断开，stopAllStations
//     不提前断开；run 收尾后最终全部断开。
func TestStopAllStationsDeviceLifecycle(t *testing.T) {
	a := newPanelTestApp(t)

	devInUse := mbus.NewMockDevice()
	devIdle := mbus.NewMockDevice()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := devInUse.Connect(ctx, "COM1"); err != nil {
		t.Fatalf("Connect devInUse: %v", err)
	}
	if err := devIdle.Connect(ctx, "COM2"); err != nil {
		t.Fatalf("Connect devIdle: %v", err)
	}
	// 工位1 正在运行并 checkout 了缓存设备（inUse=true）；工位2 空闲。
	a.stationMBUS[0] = &stationMBUS{dev: devInUse, port: "COM1", mock: true, inUse: true}
	a.stationMBUS[1] = &stationMBUS{dev: devIdle, port: "COM2", mock: true}
	a.stationRuns[0] = &stationRun{cancel: cancel, sn: "123456789012"}

	// 模拟活跃 run 的收尾：由 run 自己（bootStation 的 storeStationMBUS
	// closing 路径）断开 in-use 设备。用 release 通道控制收尾时机，验证
	// stopAllStations 不会提前 Disconnect。
	release := make(chan struct{})
	a.stationWG.Add(1)
	go func() {
		<-release
		_ = devInUse.Disconnect()
		a.stationWG.Done()
	}()

	a.stopAllStations()

	// 空闲缓存设备应立即被断开
	if _, err := devIdle.MBusReadMotor(ctx, "00"); err == nil {
		t.Error("空闲缓存设备应在 stopAllStations 时被断开")
	}
	// in-use 设备不得被 stopAllStations 提前断开（run 还在收尾）
	if _, err := devInUse.MBusReadMotor(ctx, "00"); err != nil {
		t.Error("活跃 run 正在使用的设备被提前 Disconnect")
	}

	// 放行 run 收尾：in-use 设备由 run 断开，finishStationShutdown 再清理缓存
	close(release)
	a.stationWG.Wait()
	if _, err := devInUse.MBusReadMotor(ctx, "00"); err == nil {
		t.Error("run 收尾后 in-use 设备应已断开")
	}
}

// ---------- startStation / applyConfig 并发（race） ----------

// TestStartStationApplyConfigConcurrent 用 -race 覆盖 startStation（在 a.mu
// 内深拷贝共享 env.Vars 快照）与 applyConfig（a.mu 内改写 env.Vars）的并发：
// 深拷贝与改写都必须在锁内，否则 race detector 报 map 读写竞争。
// bootStation 在 stations 槽位为 nil 时快速放弃，不触网、不开日志文件。
func TestStartStationApplyConfigConcurrent(t *testing.T) {
	a := newPanelTestApp(t)
	a.varsFile = filepath.Join(t.TempDir(), "env.yml")
	a.env.Vars["HeatNote"] = map[string]any{"mbus": map[string]any{"port": "COM9"}}
	a.plan = &config.Plan{Cases: []config.CaseItem{{Name: "dummy"}}}
	a.reg = runtime.NewRegistry() // 空注册表即可——bootStation 在 panel==nil 处放弃
	a.SetDebug(true)              // 跳过 GetTestRecord 触网

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a.applyConfig(fmt.Sprintf("COM%d", i%8+1), "fw.bin")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = a.startStation(1, fmt.Sprintf("%012d", i), "COM1")
		}
	}()
	wg.Wait()

	// 收尾：停止工位并等待所有 bootStation goroutine 退出
	a.stopAllStations()
	a.stationWG.Wait()
}

// ---------- WaitStationShutdown ----------

// TestWaitStationShutdownBlocksUntilRunsDone 验证 stopAllStations 后
// WaitStationShutdown 会阻塞到活跃 run 收尾（WG Done）才返回，且重复调用
// 安全（完成信号只关闭一次）。
func TestWaitStationShutdownBlocksUntilRunsDone(t *testing.T) {
	a := newPanelTestApp(t)

	devInUse := mbus.NewMockDevice()
	devIdle := mbus.NewMockDevice()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := devInUse.Connect(ctx, "COM1"); err != nil {
		t.Fatalf("Connect devInUse: %v", err)
	}
	if err := devIdle.Connect(ctx, "COM2"); err != nil {
		t.Fatalf("Connect devIdle: %v", err)
	}
	a.stationMBUS[0] = &stationMBUS{dev: devInUse, port: "COM1", mock: true, inUse: true}
	a.stationMBUS[1] = &stationMBUS{dev: devIdle, port: "COM2", mock: true}
	a.stationRuns[0] = &stationRun{cancel: cancel, sn: "123456789012"}

	// 模拟活跃 run 的收尾：run Done 之前 WaitStationShutdown 必须阻塞。
	release := make(chan struct{})
	a.stationWG.Add(1)
	go func() {
		<-release
		_ = devInUse.Disconnect()
		a.stationWG.Done()
	}()

	a.stopAllStations()

	waited := make(chan struct{})
	go func() {
		a.WaitStationShutdown()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("活跃 run 未收尾时 WaitStationShutdown 不应返回")
	case <-time.After(50 * time.Millisecond):
		// 正确阻塞
	}

	close(release) // 放行 run 收尾
	select {
	case <-waited:
		// run Done 后返回
	case <-time.After(3 * time.Second):
		t.Fatal("run 收尾后 WaitStationShutdown 未返回")
	}

	// 重复调用安全（完成信号已关闭，立即返回）
	a.WaitStationShutdown()

	// 收尾完成后设备应全部断开
	if _, err := devInUse.MBusReadMotor(ctx, "00"); err == nil {
		t.Error("收尾完成后 in-use 设备应已断开")
	}
	if _, err := devIdle.MBusReadMotor(ctx, "00"); err == nil {
		t.Error("收尾完成后空闲缓存设备应已断开")
	}
}

// TestWaitStationShutdownFallbackWhenNotClosing 验证 stopAllStations 从未
// 触发（异常退出路径）时，WaitStationShutdown 会补一次收尾并安全返回；
// 重复调用同样安全。
func TestWaitStationShutdownFallbackWhenNotClosing(t *testing.T) {
	a := newPanelTestApp(t)
	a.WaitStationShutdown()
	a.WaitStationShutdown() // 重复调用不阻塞
	if !a.stationsClosing {
		t.Error("WaitStationShutdown 兜底应触发 stopAllStations（置 closing）")
	}
}

// TestSwitchModeRejectDoesNotBlock 回归：switchMode 拒绝路径绝不能同步阻塞
// 主线程等待弹框回复（pump 的 fyne.Do 依赖主线程处理事件队列 → 自死锁）。
// 此处 shutdown 不关闭、无 pump：若同步调 a.Message 会永久阻塞，测试超时；
// notifyReject 放后台 goroutine 后调用立即返回。
func TestSwitchModeRejectDoesNotBlock(t *testing.T) {
	ta := test.NewApp()
	t.Cleanup(ta.Quit)
	a := &App{
		mu:                  sync.Mutex{},
		env:                 &core.Env{Vars: map[string]any{}},
		stations:            make([]*stationPanel, 3),
		stationRuns:         make([]*stationRun, 3),
		stationMBUS:         make([]*stationMBUS, 3),
		messageCh:           make(chan messageReq, 8),
		shutdown:            make(chan struct{}), // 故意不关闭：模拟无 pump 场景
		stationShutdownDone: make(chan struct{}),
	}
	a.mode = "panel"
	a.stationRuns[0] = &stationRun{sn: "123456789012"}

	done := make(chan struct{})
	go func() {
		a.switchMode(true)
		close(done)
	}()
	select {
	case <-done:
		// 拒绝路径非阻塞：立即返回
	case <-time.After(2 * time.Second):
		t.Fatal("switchMode 拒绝路径不应同步阻塞")
	}
	// 给后台 notifyReject goroutine 退路，避免测试 goroutine 泄漏
	close(a.shutdown)
}
