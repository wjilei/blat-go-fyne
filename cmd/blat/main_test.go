package main

import (
	"path/filepath"
	"testing"

	"blat/cmd/blat/cases"
	"blat/internal/config"
)

func TestBuiltinUpgradePlanUsesPanelModeAndRegisteredCase(t *testing.T) {
	const planPath = "confs/plan_PTVB1_normal_ut_upgradefirmware.yml"

	found := false
	for _, item := range builtinPlans {
		if filepath.Clean(item.Path) == filepath.Clean(planPath) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("builtinPlans 未包含 %s", planPath)
	}
	if !config.IsPanelPlan(planPath) {
		t.Fatalf("升级计划未启用 PTVB1 三工位模式: %s", planPath)
	}

	plan, err := config.LoadPlan(filepath.Join("..", "..", planPath))
	if err != nil {
		t.Fatalf("LoadPlan(%s): %v", planPath, err)
	}
	if len(plan.Cases) != 1 {
		t.Fatalf("升级计划用例数 = %d, want 1", len(plan.Cases))
	}
	const caseName = "HeatSuite::wire_valve_mbus_upgrade_firmware"
	if plan.Cases[0].Name != caseName {
		t.Fatalf("升级计划用例 = %q, want %q", plan.Cases[0].Name, caseName)
	}
	if _, err := cases.Global().Invoke(caseName); err != nil {
		t.Fatalf("升级用例未注册: %v", err)
	}
}
