package runtime

// Phase 3 B 的红：PlanRunner 应把 plan item 的自定义 args 填进
// report.CaseReport.Extra（从 item.Args 深拷贝），并按 Perl
// DisplayRole.pm:69-88 app_reports 规则删冗余：desc == title 时 desc
// 不进入 case 条目。当前 registry.go 构造 CaseReport 时不填 Extra，
// 断言 cr.Extra["蓝牙操作"] == "read" 会因 nil map 而红。

import (
	"context"
	"strings"
	"testing"
	"time"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/report"

	"gopkg.in/yaml.v3"
)

// extraCapture 记录 OnCaseStop 的 CaseReport 全文，供断言 Extra 填充。
type extraCapture struct {
	stops []report.CaseReport
}

func (c *extraCapture) OnPlanStart(total int, startTime time.Time) {}
func (c *extraCapture) OnCaseStart(seq int, cr report.CaseReport)  {}
func (c *extraCapture) OnCaseStop(seq int, cr report.CaseReport)   { c.stops = append(c.stops, cr) }
func (c *extraCapture) OnPlanStop(summary report.Summary)          {}

// TestRunPlan_ExtraInReport 验证自定义 args 平铺进 CaseReport.Extra，
// 且 desc==title 时 desc 被剔除、title 保留。
func TestRunPlan_ExtraInReport(t *testing.T) {
	reg := NewRegistry()
	reg.Register("A::B", func() (core.Case, error) { return &fakeCase{name: "B"}, nil })
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{
		{
			Name:   "A::B",
			Title:  "hello",
			Desc:   "hello", // desc == title → 按 DisplayRole 规则删除
			Counts: 1,
			Args: map[string]any{
				"蓝牙操作": "read",
				"desc":   "hello", // 显式塞进 Args，验证 cleanCaseArgs 的 delete 分支
			},
		},
	}}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}}
	rep := &extraCapture{}

	if err := pr.RunPlan(context.Background(), plan, env, rep); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if len(rep.stops) != 1 {
		t.Fatalf("caseStops = %d, want 1", len(rep.stops))
	}
	cr := rep.stops[0]

	// 自定义键必须出现在 Extra 里。
	if v, _ := cr.Extra["蓝牙操作"].(string); v != "read" {
		t.Errorf("Extra[蓝牙操作] = %v (%T), want read", cr.Extra["蓝牙操作"], cr.Extra["蓝牙操作"])
	}
	// desc==title 时 Extra 不得含 desc。
	if _, ok := cr.Extra["desc"]; ok {
		t.Errorf("Extra 不应含 desc（desc==title 时剔除）: %v", cr.Extra)
	}
	// title 独立字段不受影响。
	if cr.Title != "hello" {
		t.Errorf("Title = %q, want hello", cr.Title)
	}

	// YAML 序列化层面：无 `desc: hello`，但 `title: hello` 仍在。
	data, err := yaml.Marshal(cr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)
	if strings.Contains(out, "desc: hello") {
		t.Errorf("YAML 不应含 desc: hello（desc==title 剔除）:\n%s", out)
	}
	if !strings.Contains(out, "title: hello") {
		t.Errorf("YAML 应含 title: hello:\n%s", out)
	}
}

// TestRunPlan_ExtraKeepsDescWhenDifferent 验证 desc != title 时 Args 里的
// desc 原样保留（DisplayRole 只在 desc==title 时删）。
func TestRunPlan_ExtraKeepsDescWhenDifferent(t *testing.T) {
	reg := NewRegistry()
	reg.Register("A::B", func() (core.Case, error) { return &fakeCase{name: "B"}, nil })
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{
		{
			Name:   "A::B",
			Title:  "hello",
			Desc:   "short description",
			Counts: 1,
			Args: map[string]any{
				"蓝牙操作": "read",
				"desc":   "short description",
			},
		},
	}}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}}
	rep := &extraCapture{}

	if err := pr.RunPlan(context.Background(), plan, env, rep); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	cr := rep.stops[0]
	if v, _ := cr.Extra["desc"].(string); v != "short description" {
		t.Errorf("Extra[desc] = %v, want 保留（desc != title）: %v", cr.Extra["desc"], cr.Extra)
	}
}
