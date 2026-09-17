// Package fyneui provides a Fyne v2 implementation of core.UI and
// core.Logger.
package fyneui

import (
	"bytes"
	"context"
	"fmt"
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

// startStation 启动第 idx 号工位（idx 从 1 开始）的测试运行。
// 由 stationPanel 的 SN 输入框回车回调触发（Fyne 主线程）。
// 同步段只做锁检查 + 占位登记，重活（深拷贝 vars、打开日志、组装 reporter）
// 全部交给后台 bootStation goroutine——让 OnSubmitted 回调快速释放主线程，
// 避免 Windows 判定主消息循环"未响应"。
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
	a.stationRuns[idx-1] = placeholder
	a.mu.Unlock()

	// 后台做重活；立即返回 nil 让 OnSubmitted 释放主线程
	go a.bootStation(idx, sn, port, plan, reg, env, debug, placeholder, ctx)
	return nil
}

// bootStation 后台执行 startStation 后半段重活（深拷贝 vars、打开日志、
// 组装 reporter）+ RunPlan + 收尾。占位 placeholder 在重活完成后 CAS 替换
// 为真 run；已被 stopStation 清掉占位则放弃本次启动。
func (a *App) bootStation(idx int, sn, port string, plan *config.Plan, reg *runtime.Registry, env *core.Env, debug bool, placeholder *stationRun, ctx context.Context) {
	cancel := placeholder.cancel
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

	// 重活：深拷贝 vars（HeatNote 副本已删除 mbus_dev/bluetooth，防单跑遗留
	// 设备实例被三工位共享——串口独占冲突）+ 覆盖 HeatNote["serial"]=sn、
	// HeatNote["mbus"]["port"]=port（子 map 不存在则创建）。
	vars := deepCopyVars(env.Vars)
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

	senv := &core.Env{
		Ctx:  ctx,
		Log:  stationLog,
		UI:   newStationUI(a, fmt.Sprintf("设备%d", idx)),
		Vars: vars,
		Devs: env.Devs,
		Out:  env.Out,
	}

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

// stationMBUS 是某工位长期持有的 M-Bus 设备缓存：记录设备实例及其连接
// 参数（串口 port + mock 模式）。同参数复用同一设备（Connect 幂等不重开
// 串口），参数变化时才摘除并 Disconnect。
type stationMBUS struct {
	dev  *mbus.Device
	port string
	mock bool
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
// 设备。抓取运行列表 cancel，再从缓存摘除并锁外 Disconnect 全部设备——
// active run 若之后终态收尾，看到 closing 会断开自己的私有 mbus_dev 且不
// 重新缓存（storeStationMBUS 内部处理），不会产生泄漏或重新缓存已断开设备。
func (a *App) stopAllStations() {
	a.mu.Lock()
	a.stationsClosing = true
	runs := append([]*stationRun(nil), a.stationRuns...)
	a.mu.Unlock()
	for _, r := range runs {
		if r != nil && r.cancel != nil {
			r.cancel()
		}
	}
	// 关窗收尾：断开所有工位缓存的 M-Bus 设备（与运行中的 cancel 无耦合——
	// 缓存只保存终态写回/复用的设备，运行中新创建尚未写回的设备由终态
	// storeStationMBUS 看到 closing 后自行断开）。
	a.clearStationMBUS()
}

// switchMode 切换界面模式（single ↔ panel）。目标模式与当前一致时直接返回
// true；切换被拒绝（另一模式有运行中任务）时弹提示并返回 false，不做自动强停。
func (a *App) switchMode(isPanel bool) bool {
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
			_ = a.Message(context.Background(), "请先停止当前测试", false)
			return false
		}
	} else {
		// 切到单跑：任一工位在跑时拒绝。
		a.mu.Lock()
		busy := false
		for _, r := range a.stationRuns {
			if r != nil {
				busy = true
				break
			}
		}
		a.mu.Unlock()
		if busy {
			_ = a.Message(context.Background(), "请先停止工位测试", false)
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
