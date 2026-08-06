// Command hello is the GUI reference application for blat-go.
//
// It opens a Fyne window, loads plan.yml, and runs the plan (all cases
// auto-registered by the cases package). Pass -no-gui to run with the
// console UI instead (useful in CI / headless boxes). Pass --env to load
// a YAML env file into Env.Vars (e.g. confs/env.yml for the Heat demo).
//
// 工具栏"配置"按钮后有一个测试计划下拉框（选项见 builtinPlans），选中的
// plan.yml 路径会写入 env.Vars["HeatNote"]["plan"] 供 case 运行时判断。
// --plan 指定计划文件时，下拉框默认选中对应项；文件不在内置列表里会被
// 追加为额外选项。未传 --plan 时下拉框停在"请选择测试计划"，左侧树为空。
//
// Startup order for the GUI mode:
//  1. Build fyne UI (creates widgets, starts pump goroutine).
//  2. Hand env/registry to the UI via gui.Attach（plan 由下拉框接管，传 nil）。
//  3. gui.SetPlanList 注入计划选项并设置初始选中项（--plan 指定时预选对应项）。
//  4. Call gui.Run() which blocks on the Fyne event loop until the user
//     closes the window.
//
// The runner is no longer started from here: the toolbar Start button
// inside the UI launches it, and Stop cancels it via gui.StopRun(), so
// any in-flight Prompt/WaitContinue returns ctx.Err() and the runner
// exits cleanly.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"blat/cmd/hello/cases"
	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/device/bluetooth"
	"blat/internal/report"
	"blat/internal/runtime"
	"blat/internal/ui"
	fyneui "blat/internal/ui/fyne"
	"blat/internal/uploader"
)

// builtinPlans 是测试计划下拉框的内置选项。每项对应一个 plan.yml 文件，
// 显示名按产线实际计划自定义；新增计划只需在此追加一项。
var builtinPlans = []fyneui.PlanItem{
	{Name: "平衡阀初始化电机", Path: "confs/plan_PSAV_ut_resetvalve.yml"},
	{Name: "平衡阀检查参数", Path: "confs/plan_PSAV_ut_check_state.yml"},
	{Name: "流量计检查参数", Path: "confs/plan_PSAV_ut_check_state.yml"},
	{Name: "户控阀检查参数", Path: "confs/plan_PSAV_ut_check_state.yml"},
}

// planInList 报告 path 是否已存在于 items（按规范化路径比较）。
func planInList(items []fyneui.PlanItem, path string) bool {
	clean := filepath.Clean(path)
	for _, it := range items {
		if filepath.Clean(it.Path) == clean {
			return true
		}
	}
	return false
}

func main() {
	planPath := flag.String("plan", "", "path to plan YAML; 留空则不预选计划（下拉框停在\"请选择测试计划\"），-no-gui 模式必须提供")
	envPath := flag.String("env", "confs/env.yml", "path to vars YAML (e.g. MBUS port); 缺文件忽略")
	noGUI := flag.Bool("no-gui", false, "use console UI instead of Fyne window")
	mockBT := flag.Bool("mock-bt", false, "use mock bluetooth (no hardware); 默认 false 走真实 BLE")
	debug := flag.Bool("debug", false, "debug 模式：不实际上传 OSS / 保存数据库，把要上报的数据打印到日志")
	uploaderPath := flag.String("uploader", "confs/uploader.yml", "path to uploader credentials YAML")
	flag.Parse()

	// 上报凭据配置（OSS 日志上传 + BLAT 后台存库）：从 YAML 加载并注入
	// uploader 包。凭据是必需的，缺文件或解析失败直接退出，不构造有效
	// 上报请求继续跑。
	ucfg, err := config.LoadUploader(*uploaderPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load uploader config:", err)
		os.Exit(2)
	}
	uploader.Init(uploader.Config{
		OSS:  uploader.OSSConfig{AccessID: ucfg.OSS.AccessID, SecretKey: ucfg.OSS.SecretKey, Host: ucfg.OSS.Host, LogBucket: ucfg.OSS.LogBucket},
		Blat: uploader.BlatConfig{BaseURL: ucfg.Blat.BaseURL, Token: ucfg.Blat.Token},
	})

	// 组装下拉框计划列表：内置列表 + （若 --plan 指定了列表外的文件）额外项。
	items := append([]fyneui.PlanItem(nil), builtinPlans...)
	selectPath := ""
	var plan *config.Plan
	if *planPath != "" {
		p, err := config.LoadPlan(*planPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load plan:", err)
			os.Exit(2)
		}
		if len(p.Cases) == 0 {
			fmt.Fprintln(os.Stderr, "plan has no cases:", *planPath)
			os.Exit(2)
		}
		plan = p
		selectPath = *planPath
		if !planInList(items, *planPath) {
			items = append(items, fyneui.PlanItem{Name: filepath.Base(*planPath), Path: *planPath})
		}
	}

	vars := map[string]any{}
	if *envPath != "" {
		// 文件不存在视为"未配置"，vars 留空；其它错误才退出。
		if _, statErr := os.Stat(*envPath); statErr == nil {
			v, err := config.LoadEnv(*envPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "load env:", err)
				os.Exit(2)
			}
			vars = v
		} else if !os.IsNotExist(statErr) {
			fmt.Fprintln(os.Stderr, "stat env:", statErr)
			os.Exit(2)
		}
	}

	// 注入蓝牙 mock 开关（两分支共用）：case 端据此选择构造 mock 还是 real
	// 设备。HeatNote 键是大写且必须存在，bt_mock 存 bool。
	heatnote, _ := vars["HeatNote"].(map[string]any)
	if heatnote == nil {
		heatnote = map[string]any{}
		vars["HeatNote"] = heatnote
	}
	heatnote["bt_mock"] = *mockBT

	// 当前计划文件路径写入 env.Vars["HeatNote"]["plan"]，case 运行时据此
	// 做计划判断；未传 --plan 时删除历史残留键（GUI 模式由下拉框接管该值）。
	if *planPath != "" {
		heatnote["plan"] = *planPath
	} else {
		delete(heatnote, "plan")
	}

	if *noGUI {
		if plan == nil {
			fmt.Fprintln(os.Stderr, "-no-gui 模式必须用 --plan 指定计划文件")
			os.Exit(2)
		}
		os.Exit(runConsole(plan, vars, *debug))
	}
	os.Exit(runGUI(items, selectPath, vars, *debug))
}

func runConsole(plan *config.Plan, vars map[string]any, debug bool) int {
	c := ui.NewConsole()
	env := &core.Env{
		Ctx:  context.Background(),
		Log:  c,
		UI:   c,
		Vars: vars,
		Devs: map[string]any{"bluetooth": bluetooth.NewDevice()},
		Out:  os.Stdout,
	}
	reg := cases.Global()
	pr := runtime.NewPlanRunner(reg)
	rep := report.NewMulti(
		report.NewYAMLFile("."),
		report.NewTAP(nil),
		// hook_stop 上报：测试全部跑完后把日志压缩上传 OSS，并把测试记录
		// POST 到 BLAT 服务器数据库（对齐 Perl HeatAppUI.hook_stop）。
		// 日志取 Console 环形缓冲的完整快照；--debug 时不触网，仅打印上报数据。
		uploader.NewHookStop(env, c.SnapshotLog, debug),
	)
	if err := pr.RunPlan(context.Background(), plan, env, rep); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		disconnectBluetooth(env)
		return 1
	}
	// 对应 Perl 用例跑完释放蓝牙连接
	disconnectBluetooth(env)
	return 0
}

// disconnectBluetooth 释放 plan 跑完后遗留的蓝牙连接（对应 Perl 用例跑完
// 释放蓝牙连接）。优先取 case 存回 Vars.HeatNote["bluetooth"] 的实例（可能
// 是 case 新建后存回的），无则兜底 Devs["bluetooth"] 默认实例；断开失败忽略。
func disconnectBluetooth(env *core.Env) {
	if heatnote, _ := env.Vars["HeatNote"].(map[string]any); heatnote != nil {
		if dev, ok := heatnote["bluetooth"].(*bluetooth.Device); ok {
			_ = dev.Disconnect()
			return
		}
	}
	if dev, ok := env.Devs["bluetooth"].(*bluetooth.Device); ok {
		_ = dev.Disconnect()
	}
}

func runGUI(items []fyneui.PlanItem, selectPath string, vars map[string]any, debug bool) int {
	gui := fyneui.New("blat-go hello")

	env := &core.Env{
		Ctx:  context.Background(),
		Log:  gui,
		UI:   gui,
		Vars: vars,
		Devs: map[string]any{"bluetooth": bluetooth.NewDevice()},
		Out:  os.Stdout,
	}
	reg := cases.Global()

	// plan 的生命周期由下拉框接管：Attach 时 plan 传 nil，下拉框选中/清空
	// 时由 GUI 内部加载计划、重建用例树并写入 env.Vars["HeatNote"]["plan"]。
	gui.Attach(nil, env, reg)
	// --debug：跳过日志上传 OSS，日志以原始文本随测试记录存库。
	gui.SetDebug(debug)
	gui.SetPlanList(items, selectPath)

	// Block on the Fyne event loop. Closing the window cancels any
	// in-flight run via the SetOnClosed hook inside fyneui.New.
	gui.Run()
	return 0
}
