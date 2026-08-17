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
	// 二次检查：同步段到此处之间可能被并发占据
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
	mbus, _ := hn["mbus"].(map[string]any)
	if mbus == nil {
		mbus = map[string]any{}
	}
	mbus["port"] = port
	hn["mbus"] = mbus
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
	run.onState = func(_, new stationState, sum report.Summary) {
		panel.SetState(new, sum)
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
	a.mu.Lock()
	if a.stationRuns[idx-1] != placeholder {
		a.mu.Unlock()
		_ = logf.Close()
		cancel()
		return
	}
	a.stationRuns[idx-1] = run
	a.mu.Unlock()
	panel.BindLog(logf)

	// 跑 + 收尾（不碰 a.rows/a.tree/状态栏/蓝牙——面板模式不用）。
	err = runtime.NewPlanRunner(reg).RunPlan(ctx, plan, senv, rep)
	if err != nil {
		stationLog.Error("", err.Error())
	}
	a.mu.Lock()
	if a.stationRuns[idx-1] == run {
		a.stationRuns[idx-1] = nil
	}
	a.mu.Unlock()
	_ = logf.Close()
	cancel()
}

// stopStation 停止第 idx 号工位（idx 从 1 开始）的运行：槽位非 nil 则 cancel。
func (a *App) stopStation(idx int) {
	a.mu.Lock()
	run := a.stationRuns[idx-1]
	a.mu.Unlock()
	if run != nil && run.cancel != nil {
		run.cancel()
	}
}

// stopAllStations 停止所有工位运行（关窗时级联调用）。
func (a *App) stopAllStations() {
	a.mu.Lock()
	runs := append([]*stationRun(nil), a.stationRuns...)
	a.mu.Unlock()
	for _, r := range runs {
		if r != nil && r.cancel != nil {
			r.cancel()
		}
	}
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