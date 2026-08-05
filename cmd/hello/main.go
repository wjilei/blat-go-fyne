// Command hello is the GUI reference application for blat-go.
//
// It opens a Fyne window, loads plan.yml, and runs the plan (all cases
// auto-registered by the cases package). Pass -no-gui to run with the
// console UI instead (useful in CI / headless boxes). Pass --env to load
// a YAML env file into Env.Vars (e.g. examples/heat/vars.yml for the
// Heat demo).
//
// Startup order for the GUI mode:
//  1. Build fyne UI (creates widgets, starts pump goroutine).
//  2. Pre-fill case tree.
//  3. Hand the plan/env/registry to the UI via gui.Attach.
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

	"blat/cmd/hello/cases"
	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/device/bluetooth"
	"blat/internal/report"
	"blat/internal/runtime"
	"blat/internal/ui"
	fyneui "blat/internal/ui/fyne"
)

func main() {
	planPath := flag.String("plan", "examples/hello/plan.yml", "path to plan YAML")
	envPath := flag.String("env", "", "path to env YAML (optional)")
	noGUI := flag.Bool("no-gui", false, "use console UI instead of Fyne window")
	flag.Parse()

	plan, err := config.LoadPlan(*planPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load plan:", err)
		os.Exit(2)
	}
	if len(plan.Cases) == 0 {
		fmt.Fprintln(os.Stderr, "plan has no cases:", *planPath)
		os.Exit(2)
	}

	vars := map[string]any{}
	if *envPath != "" {
		v, err := config.LoadEnv(*envPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load env:", err)
			os.Exit(2)
		}
		vars = v
	}

	if *noGUI {
		os.Exit(runConsole(plan, vars))
	}
	os.Exit(runGUI(plan, *planPath, vars))
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

func runGUI(plan *config.Plan, planPath string, vars map[string]any) int {
	gui := fyneui.New("blat-go hello - " + planPath)

	// Pre-fill case tree.
	for _, c := range plan.Cases {
		title := c.Title
		if title == "" {
			title = c.Name
		}
		gui.AddRow(title, c.Name)
	}
	gui.SetStatus("loaded " + planPath + ", press Start to run")

	env := &core.Env{
		Ctx:  context.Background(),
		Log:  gui,
		UI:   gui,
		Vars: vars,
		Devs: map[string]any{"bluetooth": bluetooth.NewDevice()},
		Out:  os.Stdout,
	}
	reg := cases.Global()

	// Hand the runnable pieces to the UI; the toolbar Start button drives
	// the runner from there (the UI's own startRun builds the report
	// chain). We no longer start the runner at boot.
	gui.Attach(plan, env, reg)

	// Block on the Fyne event loop. Closing the window cancels any
	// in-flight run via the SetOnClosed hook inside fyneui.New.
	gui.Run()
	return 0
}
