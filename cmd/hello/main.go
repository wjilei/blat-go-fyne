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
)

// builtinPlans 是测试计划下拉框的内置选项。每项对应一个 plan.yml 文件，
// 显示名按产线实际计划自定义；新增计划只需在此追加一项。
var builtinPlans = []fyneui.PlanItem{
	{Name: "默认计划", Path: "confs/plan.yml"},
	{Name: "Heat 蓝牙示例", Path: "examples/heat/plan.yml"},
	{Name: "Hello 问候示例", Path: "examples/hello/plan.yml"},
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
	mockBT := flag.Bool("mock-bt", true, "use mock bluetooth (no hardware); set false for real BLE")
	flag.Parse()

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
		os.Exit(runConsole(plan, vars))
	}
	os.Exit(runGUI(items, selectPath, vars))
}

func runConsole(plan *config.Plan, vars map[string]any) int {
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
	rep := report.NewMulti(report.NewYAMLFile("."), report.NewTAP(nil))
	if err := pr.RunPlan(context.Background(), plan, env, rep); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		return 1
	}
	return 0
}

func runGUI(items []fyneui.PlanItem, selectPath string, vars map[string]any) int {
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
	gui.SetPlanList(items, selectPath)

	// Block on the Fyne event loop. Closing the window cancels any
	// in-flight run via the SetOnClosed hook inside fyneui.New.
	gui.Run()
	return 0
}
