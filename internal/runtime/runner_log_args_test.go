package runtime

// Phase 3 C 的红：PlanRunner 的 case_start RUNNER 日志行应把 plan args 以
// YAML 多行格式追加到 `case_start <name> ` 之后，对齐 Perl Runner.pm:119
// 的 `case_start $name ` . yaml_dump(\%tmp_arg)。当前 registry.go 只输出
// `case_start <name>`，不含 args，断言会红。
//
// 注意：yaml.v3 对 map[string]any 按 key 的 UTF-8 字节序升序输出——
// "温度偏差"(E6..) 排在 "蓝牙操作"(E8..) 之前，与 Perl hash 的随机序
// 不同但更具确定性。

import (
	"context"
	"strings"
	"testing"

	"blat/internal/config"
	"blat/internal/core"
)

// TestRunPlan_RunnerLogArgsInCaseStart 验证 case_start 行携带多行 YAML args。
func TestRunPlan_RunnerLogArgsInCaseStart(t *testing.T) {
	reg := NewRegistry()
	reg.Register("A::B", func() (core.Case, error) { return &fakeCase{name: "B"}, nil })
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{
		{
			Name:   "A::B",
			Counts: 1,
			Args: map[string]any{
				"蓝牙操作": "read",
				"温度偏差": 5,
			},
		},
	}}
	log := &logRecorder{}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}, Log: log}

	if err := pr.RunPlan(context.Background(), plan, env, nil); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	rows := log.rowsSnapshot()
	if len(rows) != 2 {
		t.Fatalf("日志行数 = %d, want 2:\n%+v", len(rows), rows)
	}
	start := rows[0]
	if start.category != "RUNNER" {
		t.Errorf("category = %q, want RUNNER", start.category)
	}
	// 多行 YAML：首行键值跟在 `case_start A::B ` 之后，后续键值换行续排。
	want := "case_start A::B 温度偏差: 5\n蓝牙操作: read"
	if !strings.Contains(start.msg, want) {
		t.Errorf("case_start 应含 args YAML %q，got:\n%q", want, start.msg)
	}
	// case_stop 行保持原格式，不被 args 污染。
	if !caseStopRe("A::B", "ok").MatchString(rows[1].msg) {
		t.Errorf("case_stop = %q, want 原格式 ^case_stop A::B ok \\d+\\.\\d{2}$", rows[1].msg)
	}
}

// TestRunPlan_RunnerLogNoArgs 验证无自定义 args 时 case_start 行不带尾随空格。
func TestRunPlan_RunnerLogNoArgs(t *testing.T) {
	reg := NewRegistry()
	reg.Register("A::B", func() (core.Case, error) { return &fakeCase{name: "B"}, nil })
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{{Name: "A::B", Counts: 1}}}
	log := &logRecorder{}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}, Log: log}

	if err := pr.RunPlan(context.Background(), plan, env, nil); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	start := log.rowsSnapshot()[0]
	if start.msg != "case_start A::B" {
		t.Errorf("无 args 时 msg = %q, want %q（无尾随空格）", start.msg, "case_start A::B")
	}
}
