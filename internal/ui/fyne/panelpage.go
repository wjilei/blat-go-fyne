// Package fyneui provides a Fyne v2 implementation of core.UI and
// core.Logger.
package fyneui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/device/mbus"
	"blat/internal/logfile"
	"blat/internal/report"
	"blat/internal/runtime"
	"blat/internal/uploader"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
)

// upgradeFirmwareCaseName 是面板升级计划必须包含的用例注册名，对齐
// cmd/blat/cases/wire_valve_mbus.go 的 Register。仅当当前 plan 含该用例时，
// 三工位才需要共享固件内容快照。
const upgradeFirmwareCaseName = "HeatSuite::wire_valve_mbus_upgrade_firmware"

// stationLogger 工位日志适配器：实现 core.Logger，把日志行写进工位 logf
// （test_P<i>.log）并刷新对应面板日志框。每工位独立实例。
type stationLogger struct {
	panel *stationPanel
	logf  *logfile.FileLogger
}

func (l *stationLogger) Info(category, msg string)  { l.write("info", category, msg) }
func (l *stationLogger) Warn(category, msg string)  { l.write("warn", category, msg) }
func (l *stationLogger) Error(category, msg string) { l.write("error", category, msg) }

// write 写一行到工位 logf 并调度面板日志框增量刷新（对齐 app.go appendLog）。
func (l *stationLogger) write(level, category, msg string) {
	if l.logf != nil {
		_ = l.logf.WriteLine(level, category, msg)
	}
	if l.panel != nil {
		fyne.Do(l.panel.RefreshLog)
	}
}

// stationTapWriter 工位 TAP writer：把 TAPReporter 的 io.Writer 输出按 \n
// 切行后写进工位 logf（复用包级 classifyTAPLine 定 level），并刷新面板日志框。
// 逻辑照 app.go tapWriter/appendTAP 参数化：partial 半行缓冲每工位独立。
type stationTapWriter struct {
	panel   *stationPanel
	logf    *logfile.FileLogger
	partial bytes.Buffer
}

func (w *stationTapWriter) Write(p []byte) (int, error) {
	w.partial.Write(p)
	full := w.partial.String()
	w.partial.Reset()
	// 按 \n 切；最后一段若无换行说明是 partial，重新塞回
	lines := strings.SplitAfter(full, "\n")
	var keep []byte
	for i, line := range lines {
		if i == len(lines)-1 && !strings.HasSuffix(line, "\n") {
			keep = []byte(line)
			continue
		}
		w.writeLine(strings.TrimRight(line, "\n"))
	}
	if len(keep) > 0 {
		w.partial.Write(keep)
	}
	return len(p), nil
}

// writeLine 把一行 TAP 文本按 classifyTAPLine 定 level 写入工位 logf。
func (w *stationTapWriter) writeLine(line string) {
	if line == "" {
		return
	}
	level := classifyTAPLine(line)
	if w.logf != nil {
		_ = w.logf.WriteLine(level, "TAP", line)
	}
	if w.panel != nil {
		fyne.Do(w.panel.RefreshLog)
	}
}

// buildPanelPage 创建"工位测试"页：3 个 stationPanel 等宽并排（GridLayout
// 三等分），启动/停止回调接到 App 的 startStation/stopStation。
func (a *App) buildPanelPage() *fyne.Container {
	a.stations = make([]*stationPanel, 3)
	a.stationRuns = make([]*stationRun, 3)
	a.stationMBUS = make([]*stationMBUS, 3)
	panels := make([]fyne.CanvasObject, 0, 3)
	for i := 1; i <= 3; i++ {
		p := newStationPanel(i)
		idx := i
		p.SetOnStart(func(sn, port string) error {
			return a.startStation(idx, sn, port)
		})
		p.SetOnStop(func() {
			a.stopStation(idx)
		})
		p.SetOnMessage(func(msg string) {
			// danger=true 用红色警示：校验失败会阻止测试启动，需要操作员正视。
			_ = a.Message(context.Background(), msg, true)
		})
		a.stations[idx-1] = p
		panels = append(panels, p.CanvasObject())
	}
	return container.New(layout.NewGridLayout(3), panels...)
}

// firmwarePathForPlan 解析当前 plan 中升级用例所需的固件路径，解析顺序与
// cmd/blat/cases/wire_valve_mbus.go 的 WireValveMBusUpgradeFirmwareCase 完全
// 一致：
//  1. plan 参数"升级文件"（中文，非空）优先；
//  2. 英文 firmware 次之；
//  3. 否则读基础 env HeatNote.mbus.firmware（GUI 配置写入的键）。
//
// 返回 (path, need)：need=false 表示当前 plan 不含升级用例，无需快照；
// need=true 时 path 为解析出的路径（可能为空串——升级 plan 未配置固件，
// 由调用方按错误处理）。
func firmwarePathForPlan(plan *config.Plan, env *core.Env) (path string, need bool) {
	if plan == nil || env == nil {
		return "", false
	}
	for _, it := range plan.Cases {
		if it.Name != upgradeFirmwareCaseName {
			continue
		}
		if v, ok := it.Args["升级文件"].(string); ok && v != "" {
			return v, true
		}
		// 英文 firmware：空串视作未配置（与 Case 的 Configure 一致——空路径
		// 时 Run 回退 HeatNote.mbus.firmware），继续向下回退。
		if v, ok := it.Args["firmware"].(string); ok && v != "" {
			return v, true
		}
		if hn, ok := env.Vars["HeatNote"].(map[string]any); ok {
			if m, ok := hn["mbus"].(map[string]any); ok {
				if v, ok := m["firmware"].(string); ok {
					return v, true
				}
			}
		}
		return "", true // 升级 plan 但没配路径：need=true，调用方报错
	}
	return "", false
}

// loadFirmwareSnapshot 在持有 a.mu 时调用：按当前 plan 决定是否需要固件
// 快照并准备它。
//   - 非升级 plan：清空缓存，返回 nil（不要求固件文件存在）。
//   - 升级 plan 且缓存命中（同 plan 同路径）：直接复用，不重新读盘。
//   - 升级 plan 且路径/plan 变化：读盘一次 + 计算 SHA-256 重建快照；
//     读取失败、路径为空、非普通文件返回明确错误并清空缓存（升级 plan
//     不允许三工位各自跳过）。
//
// 快照 Data 不写入 Vars——三工位通过各自 senv.Firmware 共享同一指针。
func (a *App) loadFirmwareSnapshot() error {
	path, need := firmwarePathForPlan(a.plan, a.env)
	if !need {
		a.clearFirmwareSnapshot()
		return nil
	}
	if a.fwSnap != nil && a.fwSnapPlan == a.plan && a.fwSnapPath == path {
		return nil // 同 plan 同路径：跨工位/跨轮次复用
	}
	if path == "" {
		a.clearFirmwareSnapshot()
		return fmt.Errorf("升级计划未配置固件文件（请配置 升级文件/firmware 参数或 HeatNote.mbus.firmware）")
	}
	fi, err := os.Stat(path)
	if err != nil {
		a.clearFirmwareSnapshot()
		return fmt.Errorf("固件文件读取失败: %w", err)
	}
	if !fi.Mode().IsRegular() {
		a.clearFirmwareSnapshot()
		return fmt.Errorf("固件路径不是普通文件: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		a.clearFirmwareSnapshot()
		return fmt.Errorf("固件文件读取失败: %w", err)
	}
	sum := sha256.Sum256(data)
	a.fwSnap = &core.FirmwareSnapshot{
		Path:   path,
		Data:   data,
		Size:   int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]),
	}
	a.fwSnapPlan = a.plan
	a.fwSnapPath = path
	return nil
}

// clearFirmwareSnapshot 清空面板固件快照缓存并释放 Data 引用。配置变更、
// 成功换 plan、清 plan、关窗时调用；必须在持有 a.mu 时调用。
func (a *App) clearFirmwareSnapshot() {
	a.fwSnap = nil
	a.fwSnapPlan = nil
	a.fwSnapPath = ""
}

// startStation 启动第 idx 号工位（idx 从 1 开始）的测试运行。
// 由 stationPanel 的 SN 输入框回车回调触发（Fyne 主线程）。
// 同步段在 a.mu 内完成锁检查 + 共享 env.Vars 深拷贝快照 + 占位登记；
// 其余重活（打开日志、组装 reporter）交给后台 bootStation goroutine——
// 让 OnSubmitted 回调快速释放主线程，避免 Windows 判定主消息循环"未响应"。
// vars 快照必须在锁内生成：applyConfig 可能并发改写共享 env.Vars。
func (a *App) startStation(idx int, sn, port string) error {
	// 槽位占用检查 + 取共享资源（mu 保护）
	a.mu.Lock()
	if idx < 1 || idx > len(a.stationRuns) {
		a.mu.Unlock()
		return fmt.Errorf("无效工位号 %d", idx)
	}
	if a.stationsClosing {
		a.mu.Unlock()
		return fmt.Errorf("应用正在关闭")
	}
	if a.stationRuns[idx-1] != nil {
		a.mu.Unlock()
		return fmt.Errorf("设备%d 正在运行", idx)
	}
	plan, reg, env, debug := a.plan, a.reg, a.env, a.debug
	a.mu.Unlock()
	if plan == nil || env == nil || reg == nil {
		return fmt.Errorf("没有可运行的 plan，请先加载")
	}
	if len(plan.Cases) == 0 {
		return fmt.Errorf("plan 为空")
	}

	// 独立 ctx + 占位 stationRun（带 cancel，让 stopStation 在重活阶段也能中止）
	ctx, cancel := context.WithCancel(context.Background())
	placeholder := &stationRun{cancel: cancel, sn: sn}
	a.mu.Lock()
	// 二次检查：同步段到此处之间可能被并发占据，或应用已开始关闭
	//（closing 后 bootStation 会静默放弃，不能让 UI 短暂进入运行中）。
	if a.stationsClosing {
		a.mu.Unlock()
		cancel()
		return fmt.Errorf("应用正在关闭")
	}
	if a.stationRuns[idx-1] != nil {
		a.mu.Unlock()
		cancel()
		return fmt.Errorf("设备%d 正在运行", idx)
	}
	// 三面板序列号互斥：已有其它工位测试同一 SN 时拒绝（排除自己——
	// 同一工位重复 SN 允许，重启测试）。占位阶段即检查，用户连续输入不漏。
	for i, r := range a.stationRuns {
		if i == idx-1 {
			continue
		}
		if r != nil && r.sn == sn {
			a.mu.Unlock()
			cancel()
			return fmt.Errorf("设备%d 序列号 %s 正在测试（设备%d）", idx, sn, i+1)
		}
	}
	// 固件快照：仅升级 plan 需要。第一个工位在登记占位前、a.mu 内读盘一次
	// 并计算 SHA-256，后续工位/轮次复用同一快照（跨工位共享同一指针）；
	// 读取失败/未配置/非普通文件返回明确错误，不登记占位、不启动任何槽位
	//（升级 plan 不允许三路各自跳过）。
	if err := a.loadFirmwareSnapshot(); err != nil {
		a.mu.Unlock()
		cancel()
		return err
	}
	// 在锁内生成工位私有 vars 快照：applyConfig 可能并发改写共享 env.Vars
	//（配置弹框保存），深拷贝必须在 a.mu 内完成，防止 bootStation 无锁遍历
	// 共享 map 造成数据竞争（go test -race 可检出）。工位各自的 serial/port
	// 覆盖由 bootStation 在快照上完成。fwSnap 在锁内捕获：快照对象不可变，
	// 三工位共享同一指针是安全的。
	fwSnap := a.fwSnap
	vars := deepCopyVars(env.Vars)
	a.stationRuns[idx-1] = placeholder
	a.stationWG.Add(1)
	a.mu.Unlock()

	// 后台做重活；立即返回 nil 让 OnSubmitted 释放主线程
	go a.bootStation(idx, sn, port, plan, reg, vars, fwSnap, env, debug, placeholder, ctx)
	return nil
}

// bootStation 后台执行 startStation 后半段重活（在私有 vars 快照上覆盖
// serial/port、打开日志、组装 reporter）+ RunPlan + 收尾。占位 placeholder
// 在重活完成后 CAS 替换为真 run；已被 stopStation 清掉占位则放弃本次启动。
// vars 是 startStation 在 a.mu 内生成的深拷贝快照，工位私有，可自由读写；
// fwSnap 是 startStation 在 a.mu 内捕获的共享固件快照（升级 plan 专用，
// 非升级 plan 为 nil），三工位指向同一份 Data/SHA256。
func (a *App) bootStation(idx int, sn, port string, plan *config.Plan, reg *runtime.Registry, vars map[string]any, fwSnap *core.FirmwareSnapshot, env *core.Env, debug bool, placeholder *stationRun, ctx context.Context) {
	cancel := placeholder.cancel
	// 关窗收尾（stopAllStations）等待所有工位 goroutine 退出的记账点
	defer a.stationWG.Done()
	if idx < 1 || idx > len(a.stations) {
		cancel()
		return
	}
	panel := a.stations[idx-1]
	if panel == nil {
		a.mu.Lock()
		if a.stationRuns[idx-1] == placeholder {
			a.stationRuns[idx-1] = nil
		}
		a.mu.Unlock()
		cancel()
		return
	}

	// 重活：在私有快照上覆盖 HeatNote["serial"]=sn、
	// HeatNote["mbus"]["port"]=port（子 map 不存在则创建）。快照已删除
	// mbus_dev/bluetooth，防单跑遗留设备实例被三工位共享——串口独占冲突。
	if vars == nil {
		vars = map[string]any{}
	}
	hn, _ := vars["HeatNote"].(map[string]any)
	if hn == nil {
		hn = map[string]any{}
	}
	hn["serial"] = sn
	mbusCfg, _ := hn["mbus"].(map[string]any)
	if mbusCfg == nil {
		mbusCfg = map[string]any{}
	}
	mbusCfg["port"] = port
	hn["mbus"] = mbusCfg
	vars["HeatNote"] = hn

	// 打开工位日志文件并清空（每次工位启动清空该工位日志，对齐 startRun）。
	logf, err := logfile.Open(config.DefaultPanelLogPath(idx))
	if err != nil {
		a.mu.Lock()
		if a.stationRuns[idx-1] == placeholder {
			a.stationRuns[idx-1] = nil
		}
		a.mu.Unlock()
		cancel()
		panel.SetState(stFail, report.Summary{}) // 失败回滚 UI
		_ = a.Message(context.Background(), fmt.Sprintf("设备%d：打开日志失败：%v", idx, err), true)
		return
	}
	if err := logf.Truncate(); err != nil {
		_ = logf.Close()
		a.mu.Lock()
		if a.stationRuns[idx-1] == placeholder {
			a.stationRuns[idx-1] = nil
		}
		a.mu.Unlock()
		cancel()
		panel.SetState(stFail, report.Summary{})
		_ = a.Message(context.Background(), fmt.Sprintf("设备%d：清空日志失败：%v", idx, err), true)
		return
	}

	// 仿 queryRecordThenRun：用 SN 查整机测试记录写入工位 vars["HeatNote"]，
	// 避免基础 env PSAV 残留字段（pn/lot/model/tenant_id 等）污染 PTVB1
	// 工位上报。失败时弹框提示 + abort（与单跑模式 queryRecordThenRun 失败
	// 语义一致，不降级继续启动）。bootStation 在后台 goroutine，触网不阻塞
	// 主线程；--debug 跳过。
	if !a.isDebug() {
		rec, err := uploader.GetTestRecord(sn)
		if err != nil {
			fyne.Do(func() {
				dialog.ShowError(fmt.Errorf("设备%d 查询测试记录失败: %w", idx, err), a.win)
			})
			a.mu.Lock()
			if a.stationRuns[idx-1] == placeholder {
				a.stationRuns[idx-1] = nil
			}
			a.mu.Unlock()
			cancel()
			_ = logf.Close()
			panel.SetState(stFail, report.Summary{})
			return
		}
		applyTestRecordTo(vars, rec)
	}
	// 同步 test_mode：loadPlanByPath 已按当前计划把 test_mode 写入基础 env
	// （setTestModeFromPlan），这里拷到工位 vars，避免工位上报缺 test_mode
	// 或带 PSAV 残留模式。
	a.mu.Lock()
	var baseHN map[string]any
	if a.env != nil {
		baseHN, _ = a.env.Vars["HeatNote"].(map[string]any)
	}
	a.mu.Unlock()
	if baseHN != nil {
		if tm, ok := baseHN["test_mode"]; ok {
			hn["test_mode"] = tm
		}
	}

	// 独立 env：Ctx=ctx、Vars=vars、Devs 浅拷贝、Log=工位 logger、
	// UI=stationUI（文案加【设备N】前缀）、Out 沿用。
	stationLog := &stationLogger{panel: panel, logf: logf}

	// M-Bus 缓存复用：同工位同串口同 mock 模式时把缓存的已连接设备注入
	// 私有 mbus_dev，_ensureMBUS 直接复用（Connect 幂等不重开串口——避免
	// 每轮新建 Device 重开同一 COM 口报 Access is denied）；配置变化则先
	// 摘除并 Disconnect 旧设备（stationMBUSFor 内做），本轮 case 自行新建，
	// 终态时由 storeStationMBUS 写回缓存供下轮。
	mbusMock, _ := hn["mbus_mock"].(bool)
	if cached, hit, changed := a.stationMBUSFor(idx, port, mbusMock); hit {
		hn["mbus_dev"] = cached
		if a.isDebug() {
			stationLog.Info("", fmt.Sprintf("设备%d bootStation: 复用 M-Bus 连接 %s", idx, port))
		}
	} else if changed {
		// 缓存存在但串口/mock 配置变化：stationMBUSFor 已摘除并 Disconnect
		// 旧设备，本轮 case 将按新配置新建。
		if a.isDebug() {
			stationLog.Info("", fmt.Sprintf("设备%d bootStation: M-Bus 配置变化，断开旧连接", idx))
		}
	}

	// 独立 env：Ctx=ctx、Vars=vars、Devs 浅拷贝、Log=工位 logger、
	// UI=stationUI（文案加【设备N】前缀）、Out 沿用；Firmware 指向共享
	// 固件快照（升级 plan 专用，可为 nil）。
	senv := newStationEnv(
		ctx, vars, env.Devs, env.Out,
		stationLog, newStationUI(a, fmt.Sprintf("设备%d", idx)),
		fwSnap,
	)

	// 组装 stationRun（logOff/logGen 初始 0；plan/reg 与单跑共享只读）。
	run := &stationRun{
		ctx:    ctx,
		cancel: cancel,
		env:    senv,
		logf:   logf,
		logOff: 0,
		logGen: 0,
		plan:   plan,
		reg:    reg,
		sn:     sn,
	}
	// 状态接线：stationAdapter 的 SetState 迁移经 run.onState 回调
	// 触发对应面板 SetState（状态灯/状态文字/控件可用性）。
	//
	// 关键：终态（done/fail/stopped）只保存在 run 里，绝不在此开放
	// stationRuns 槽位，也不在此写缓存/关日志——OnPlanStop 触发时 RunPlan
	// 尚未完整返回，report 链中排在 stationAdapter 之后的 reporters（包括
	// HookStop 上报）以及 logf.Close 都还没执行；若此刻清空槽位，同一工位
	// 第二轮会在这之前启动，两个 run 并发写同一固定日志文件（test_P<i>.log）
	// 导致串轮。终态 UI 与槽位清空统一延后到 bootStation 收尾末尾（见
	// RunPlan 返回后代码）。running 状态仍即时更新，让操作员立刻看到"运行中"。
	run.onState = func(_, new stationState, sum report.Summary) {
		if new == stRunning {
			panel.SetState(new, sum)
		}
	}

	// reporter：YAML(工位报告) + TAP(工位日志框) + stationAdapter(状态机)
	// + HookStop(工位独立上报)。panelIdx=idx：OSS 路径加 _P<i> 后缀，
	// 避免三工位并发完成同秒上传互相覆盖。
	rep := report.NewMulti(
		report.NewYAMLPath(config.DefaultPanelReportPath(idx)).
			WithLogfile(logf).
			WithVars(vars),
		report.NewTAP(&stationTapWriter{panel: panel, logf: logf}),
		&stationAdapter{run: run},
		uploader.NewHookStop(senv, logf.Snapshot, debug, idx),
	)

	// CAS：占位还在才替换为真 run；已被 stopStation 清掉则放弃本次启动。
	// closing 时也不得继续（startStation 已拒绝新启动，此处覆盖占位登记后
	// 才关窗的竞态窗口）。放弃路径需清占位、cancel、close logf，若已复用
	// 缓存设备（注入 mbus_dev）则一并断开（关窗时 clearStationMBUS 也会
	// 断开它，幂等）。注意：closing 只在持锁时读取一次，避免未加锁读。
	a.mu.Lock()
	closing := a.stationsClosing
	if closing || a.stationRuns[idx-1] != placeholder {
		if a.stationRuns[idx-1] == placeholder {
			a.stationRuns[idx-1] = nil
		}
		a.mu.Unlock()
		if closing {
			if hn, ok := vars["HeatNote"].(map[string]any); ok {
				if d, ok := hn["mbus_dev"].(*mbus.Device); ok && d != nil {
					_ = d.Disconnect()
				}
			}
		}
		_ = logf.Close()
		cancel()
		return
	}
	a.stationRuns[idx-1] = run
	a.mu.Unlock()
	panel.BindLog(logf)

	if a.isDebug() {
		stationLog.Info("", fmt.Sprintf("设备%d bootStation: RunPlan 开始", idx))
	}

	// 跑 + 收尾（不碰 a.rows/a.tree/状态栏/蓝牙——面板模式不用）。
	err = runtime.NewPlanRunner(reg).RunPlan(ctx, plan, senv, rep)
	if a.isDebug() {
		stationLog.Info("", fmt.Sprintf("设备%d bootStation: RunPlan 返回 err=%v", idx, err))
	}
	if err != nil {
		stationLog.Error("", err.Error())
	}

	// 终态收尾（顺序关键，避免同一工位第二轮与上一轮收尾并发）：
	//   1) M-Bus 设备写回缓存（应用关闭时 storeStationMBUS 改为直接断开）；
	//   2) 关闭工位日志文件——下一轮启动前句柄必须已释放，否则两个 run
	//      并发写同一 test_P<i>.log；
	//   3) cancel 释放运行资源；
	//   4) 以上全部完成后才在 a.mu 下 CAS 清空 stationRuns 槽位；
	//   5) 最后才把面板切到终态——UI 一旦显示完成/失败/已停止，说明日志
	//      句柄已关闭、缓存已就绪、槽位已清空，操作员可立即开始下一轮。
	// 终态 state/summary 由 stationAdapter.OnPlanStop 的 SetState 写入 run，
	// 这里从 run 读取后再统一展示。
	if a.storeStationMBUS(idx, vars, port, mbusMock) {
		if a.isDebug() {
			stationLog.Info("", fmt.Sprintf("设备%d bootStation: M-Bus 缓存已更新 %s", idx, port))
		}
	}
	_ = logf.Close()
	cancel()

	run.mu.Lock()
	finalState := run.state
	finalSum := run.summary
	run.mu.Unlock()
	a.mu.Lock()
	if a.stationRuns[idx-1] == run {
		a.stationRuns[idx-1] = nil
	}
	a.mu.Unlock()
	panel.SetState(finalState, finalSum)
}

// newStationEnv 组装工位私有 core.Env：vars 是 startStation 生成的深拷贝
// 快照（工位私有，可自由读写）；fwSnap 是三工位共享的固件内容快照（升级
// plan 专用，非升级 plan 为 nil）。抽成纯函数便于单测验证 Firmware 接线。
func newStationEnv(ctx context.Context, vars map[string]any, devs map[string]any, out io.Writer, log core.Logger, ui core.UI, fwSnap *core.FirmwareSnapshot) *core.Env {
	return &core.Env{
		Ctx:      ctx,
		Log:      log,
		UI:       ui,
		Vars:     vars,
		Devs:     devs,
		Out:      out,
		Firmware: fwSnap,
	}
}

// stationMBUS 是某工位长期持有的 M-Bus 设备缓存：记录设备实例及其连接
// 参数（串口 port + mock 模式）。同参数复用同一设备（Connect 幂等不重开
// 串口），参数变化时才摘除并 Disconnect。
type stationMBUS struct {
	dev  *mbus.Device
	port string
	mock bool
	// inUse 标记该缓存设备正被某个活跃 run 复用（stationMBUSFor 命中时置位，
	// storeStationMBUS 写回新条目时清位）。受 a.mu 保护。stopAllStations 关闭
	// 时跳过 inUse 设备——由运行中的 bootStation 收尾断开，避免活跃事务期间
	// 提前 Disconnect；未被活跃 run 使用的缓存设备可立即关闭。
	inUse bool
}

// stationMBUSFor 按工位 idx（1..3）查询 M-Bus 缓存：
//   - 应用关闭（stationsClosing）：不返回任何设备（返回 hit=false、
//     changed=false），也不摘除缓存——clearStationMBUS 统一收尾。
//   - 缓存存在且 port/mock 与请求一致：返回 (dev, hit=true, changed=false)，
//     调用方把 dev 注入私有 vars["HeatNote"]["mbus_dev"] 让 _ensureMBUS
//     直接复用（Connect 幂等，不重开串口）。
//   - 缓存存在但 port/mock 变化：从缓存摘除并在锁外 Disconnect 旧 dev
//     （释放旧串口句柄），返回 (nil, hit=false, changed=true)——本轮 case
//     自行创建新设备，运行结束后再由 storeStationMBUS 写回缓存。
//   - 无缓存：返回 (nil, hit=false, changed=false)。
//
// 只操作工位私有缓存，绝不碰共享 env.Devs（main 未注入 mbus）。
func (a *App) stationMBUSFor(idx int, port string, mock bool) (dev *mbus.Device, hit, changed bool) {
	a.mu.Lock()
	if idx < 1 || idx > len(a.stationMBUS) {
		a.mu.Unlock()
		return nil, false, false
	}
	if a.stationsClosing {
		a.mu.Unlock()
		return nil, false, false
	}
	c := a.stationMBUS[idx-1]
	if c == nil {
		a.mu.Unlock()
		return nil, false, false
	}
	if c.port == port && c.mock == mock {
		// 命中：设备交给本工位 run 使用，标记 inUse——stopAllStations 关窗
		// 时不再立即断开它，改由该 run 的收尾（storeStationMBUS closing 路径）
		// 断开，避免活跃事务期间提前 Disconnect。
		c.inUse = true
		a.mu.Unlock()
		return c.dev, true, false
	}
	// 配置变化：摘除缓存，锁外 Disconnect 旧设备
	a.stationMBUS[idx-1] = nil
	a.mu.Unlock()
	if c.dev != nil {
		_ = c.dev.Disconnect()
	}
	return nil, false, true
}

// storeStationMBUS 把工位私有 vars 里的 *mbus.Device 写回该工位缓存，
// 供下一轮复用。只在终态收尾时调用：此时 RunPlan 已结束，设备句柄仍持有，
// 下一轮 deepCopyVars 删掉 mbus_dev 后能从这里重新注入同一设备。正常完成/
// 失败/用户停止都保留连接。
//
// 应用关闭（stationsClosing）时不写缓存，改为直接锁外 Disconnect 设备
// （释放串口）；返回 false。无设备（本轮 plan 未使用 M-Bus 或类型不对）也
// 返回 false。成功写回缓存返回 true。
func (a *App) storeStationMBUS(idx int, vars map[string]any, port string, mock bool) bool {
	hn, _ := vars["HeatNote"].(map[string]any)
	if hn == nil {
		return false
	}
	dev, ok := hn["mbus_dev"].(*mbus.Device)
	if !ok || dev == nil {
		return false
	}
	a.mu.Lock()
	if idx < 1 || idx > len(a.stationMBUS) {
		a.mu.Unlock()
		return false
	}
	if a.stationsClosing {
		// 应用关闭：不再缓存，断开设备（锁外 Disconnect）
		a.mu.Unlock()
		_ = dev.Disconnect()
		return false
	}
	a.stationMBUS[idx-1] = &stationMBUS{dev: dev, port: port, mock: mock}
	a.mu.Unlock()
	return true
}

// clearStationMBUS 摘除全部工位的 M-Bus 缓存并在锁外 Disconnect 设备，
// 释放所有串口句柄。关窗（stopAllStations）时调用。
func (a *App) clearStationMBUS() {
	a.mu.Lock()
	cached := a.stationMBUS
	a.stationMBUS = make([]*stationMBUS, len(cached))
	a.mu.Unlock()
	for _, c := range cached {
		if c != nil && c.dev != nil {
			_ = c.dev.Disconnect()
		}
	}
}

// stopStation 停止第 idx 号工位（idx 从 1 开始）的运行：槽位非 nil 则 cancel。
func (a *App) stopStation(idx int) {
	a.mu.Lock()
	if idx < 1 || idx > len(a.stationRuns) {
		a.mu.Unlock()
		return
	}
	run := a.stationRuns[idx-1]
	a.mu.Unlock()
	if run != nil && run.cancel != nil {
		run.cancel()
	}
}

// stopAllStations 停止所有工位运行（关窗时级联调用）。先置 stationsClosing
// 标记：此后 startStation 拒绝新启动、storeStationMBUS 不再写缓存而直接断开
// 设备。然后：
//   - 立即断开"未被活跃 run 使用"的缓存设备（inUse=false，空闲缓存）；
//   - 正在被活跃 run 复用的设备（inUse=true）**不在此断开**——由对应
//     bootStation 的收尾（storeStationMBUS 看到 closing 后直接 Disconnect）
//     断开，避免 cancel 后立即 Disconnect 活跃事务正在使用的设备；
//   - 收尾等待放后台 goroutine（finishStationShutdown）：等所有工位 goroutine
//     退出后再统一清缓存。等待不能阻塞 Fyne 主线程——bootStation 收尾里的
//     panel.SetState 经 fyne.Do 需要主线程处理，主线程被阻塞会死锁。
func (a *App) stopAllStations() {
	a.mu.Lock()
	a.stationsClosing = true
	// 应用关闭：释放固件快照引用（Data 由进程退出兜底回收）。
	a.clearFirmwareSnapshot()
	runs := append([]*stationRun(nil), a.stationRuns...)
	// 摘除并断开未被活跃 run 使用的缓存设备；inUse 设备留给 run 收尾
	var idle []*mbus.Device
	for i, c := range a.stationMBUS {
		if c != nil && !c.inUse {
			idle = append(idle, c.dev)
			a.stationMBUS[i] = nil
		}
	}
	a.mu.Unlock()
	for _, r := range runs {
		if r != nil && r.cancel != nil {
			r.cancel()
		}
	}
	for _, d := range idle {
		if d != nil {
			_ = d.Disconnect()
		}
	}
	go a.finishStationShutdown()
}

// finishStationShutdown 等待所有工位 goroutine（bootStation，含占位放弃路径）
// 退出后，统一摘除并断开剩余缓存设备，最后关闭完成信号
// （stationShutdownDone，经 Once 只关一次）。在后台 goroutine 中执行，避免
// 阻塞 Fyne 主线程（bootStation 收尾的 fyne.Do 需要主线程处理）。
func (a *App) finishStationShutdown() {
	a.stationWG.Wait()
	a.clearStationMBUS()
	a.stationShutdownOnce.Do(func() { close(a.stationShutdownDone) })
}

// WaitStationShutdown 等待关窗后全部工位 goroutine 退出并完成缓存清理。
// 供 main 在 ShowAndRun() 返回后调用——此时事件循环已结束、主线程空闲，
// 阻塞等待不会卡住 bootStation 收尾的 fyne.Do。重复调用安全（完成信号
// 只关闭一次）。若 stopAllStations 尚未被触发（异常退出路径），先补一次
// 收尾，避免永久阻塞。
func (a *App) WaitStationShutdown() {
	a.mu.Lock()
	closing := a.stationsClosing
	a.mu.Unlock()
	if !closing {
		a.stopAllStations()
	}
	<-a.stationShutdownDone
}

// stationsBusy 返回任一工位正在运行（含占位 boot 阶段）。此时不允许修改
// 配置或切换计划：运行中的工位已按旧全局值启动，改动会让三工位拿到不一致
// 的全局配置（firmware/串口），且运行中覆盖 a.plan 会破坏下一轮语义。
func (a *App) stationsBusy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.stationRuns {
		if r != nil {
			return true
		}
	}
	return false
}

// notifyReject 是主线程拒绝路径的非阻塞提示：switchMode/onPlanSelected 等
// 由计划下拉框回调（Fyne 主线程）触发，若直接同步调 a.Message 会自死锁——
// a.Message 等待弹框 reply，而 pump 弹框经 fyne.Do 依赖主线程处理事件队列，
// 主线程被自己阻塞。故放后台 goroutine；shutdown 关闭时 Message 立即返回
// （无 goroutine 泄漏），测试环境同样安全。
func (a *App) notifyReject(msg string) {
	go func() {
		_ = a.Message(context.Background(), msg, false)
	}()
}

// switchMode 切换界面模式（single ↔ panel）。目标模式与当前一致时直接返回
// true；切换被拒绝（另一模式有运行中任务）时弹提示并返回 false，不做自动强停。
//
// 面板目标（含 panel→panel 换计划）：任一工位含占位 boot 或正在运行都拒绝，
// 防止运行中覆盖全局 a.plan——运行中的工位各自持有启动时的 plan 快照，切走
// 会让 UI 展示的全局计划与运行不一致。
func (a *App) switchMode(isPanel bool) bool {
	if isPanel && a.stationsBusy() {
		a.notifyReject("请先停止工位测试")
		return false
	}
	a.mu.Lock()
	cur := a.mode
	a.mu.Unlock()
	target := "single"
	if isPanel {
		target = "panel"
	}
	if cur == target {
		return true
	}
	if isPanel {
		// 切到面板：单跑模式有活跃 run 时拒绝（查 runCtx 机制，见 running()）。
		if a.running() {
			a.notifyReject("请先停止当前测试")
			return false
		}
	} else {
		// 切到单跑：任一工位在跑时拒绝。
		if a.stationsBusy() {
			a.notifyReject("请先停止工位测试")
			return false
		}
	}
	a.mu.Lock()
	a.mode = target
	a.mu.Unlock()
	if a.mainTabs != nil {
		idx := 0
		if isPanel {
			idx = 1
		}
		fyne.Do(func() { a.mainTabs.SelectTabIndex(idx) })
	}
	return true
}
