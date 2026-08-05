package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/report"
)

// fakeCase always returns its configured error (nil => pass).
type fakeCase struct {
	name string
	err  error
}

func (f *fakeCase) Name() string { return f.name }
func (f *fakeCase) Run(ctx context.Context, env *core.Env) error {
	return f.err
}

// captureReporter records the event sequence of a plan run.
type captureReporter struct {
	planStarts  int
	caseStarts  []int
	caseStops   []int
	caseResults []report.CaseResult
	planStops   int
	summary     report.Summary
}

func (c *captureReporter) OnPlanStart(total int, startTime time.Time) {
	c.planStarts++
}
func (c *captureReporter) OnCaseStart(seq int, cr report.CaseReport) {
	c.caseStarts = append(c.caseStarts, seq)
}
func (c *captureReporter) OnCaseStop(seq int, cr report.CaseReport) {
	c.caseStops = append(c.caseStops, seq)
	c.caseResults = append(c.caseResults, cr.Result)
}
func (c *captureReporter) OnPlanStop(summary report.Summary) {
	c.planStops++
	c.summary = summary
}

func TestRunPlan_ReportsEventOrder(t *testing.T) {
	reg := NewRegistry()
	reg.Register("HelloSuite::SayHello", func() (core.Case, error) {
		return &fakeCase{name: "SayHello"}, nil
	})
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{
		{Name: "HelloSuite::SayHello", Title: "a", Counts: 1},
		{Name: "HelloSuite::SayHello", Title: "b", Counts: 2},
	}}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}}
	rep := &captureReporter{}

	if err := pr.RunPlan(context.Background(), plan, env, rep); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	if rep.planStarts != 1 {
		t.Errorf("planStarts = %d, want 1", rep.planStarts)
	}
	if rep.planStops != 1 {
		t.Errorf("planStops = %d, want 1", rep.planStops)
	}
	// 3 attempts total: seq 1 (counts 1) + seq 2,3 (counts 2).
	want := []int{1, 2, 3}
	if len(rep.caseStarts) != 3 || !equalInts(rep.caseStarts, want) {
		t.Errorf("caseStarts = %v, want %v", rep.caseStarts, want)
	}
	if len(rep.caseStops) != 3 || !equalInts(rep.caseStops, want) {
		t.Errorf("caseStops = %v, want %v", rep.caseStops, want)
	}
	for i, r := range rep.caseResults {
		if r != report.CaseOK {
			t.Errorf("caseResults[%d] = %q, want ok", i, r)
		}
	}
	if rep.summary.TotalNum != 3 {
		t.Errorf("summary.TotalNum = %d, want 3", rep.summary.TotalNum)
	}
	if rep.summary.OKNum != 3 || rep.summary.FailNum != 0 {
		t.Errorf("summary = ok:%d fail:%d, want ok:3 fail:0", rep.summary.OKNum, rep.summary.FailNum)
	}
	if rep.summary.Result != "pass" {
		t.Errorf("summary.Result = %q, want pass", rep.summary.Result)
	}
}

func TestRunPlan_FailReportsSummary(t *testing.T) {
	boom := errors.New("boom")
	reg := NewRegistry()
	reg.Register("F::C", func() (core.Case, error) {
		return &fakeCase{name: "C", err: boom}, nil
	})
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{{Name: "F::C", Counts: 1}}}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}}
	rep := &captureReporter{}

	err := pr.RunPlan(context.Background(), plan, env, rep)
	if err == nil {
		t.Fatal("want error from failing case")
	}
	if len(rep.caseStops) != 1 || rep.caseResults[0] != report.CaseFail {
		t.Errorf("caseStops/results = %v %v, want fail for seq 1", rep.caseStops, rep.caseResults)
	}
	if rep.summary.FailNum != 1 || rep.summary.Result != "fail" {
		t.Errorf("summary = %+v, want failNum 1 result fail", rep.summary)
	}
	if rep.summary.Reason == "" {
		t.Errorf("summary.Reason empty, want failure reason")
	}
}

func TestRunPlan_InvokeFailureReports(t *testing.T) {
	reg := NewRegistry() // nothing registered
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{{Name: "Missing::Case", Counts: 1}}}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}}
	rep := &captureReporter{}

	if err := pr.RunPlan(context.Background(), plan, env, rep); err == nil {
		t.Fatal("want error for unregistered case")
	}
	if len(rep.caseStops) != 1 || rep.caseResults[0] != report.CaseFail {
		t.Errorf("caseStops/results = %v %v, want one fail", rep.caseStops, rep.caseResults)
	}
	if rep.planStops != 1 {
		t.Errorf("planStops = %d, want 1 on failure path", rep.planStops)
	}
}

func TestRunPlan_NilReporter(t *testing.T) {
	reg := NewRegistry()
	reg.Register("A::B", func() (core.Case, error) {
		return &fakeCase{name: "B"}, nil
	})
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{{Name: "A::B", Counts: 1}}}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}}

	if err := pr.RunPlan(context.Background(), plan, env, nil); err != nil {
		t.Fatalf("RunPlan with nil reporter: %v", err)
	}
}

func TestRunPlan_InvalidArgs(t *testing.T) {
	pr := NewPlanRunner(NewRegistry())
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}}
	if err := pr.RunPlan(context.Background(), nil, env, nil); err == nil {
		t.Fatal("want error for nil plan")
	}
	if err := pr.RunPlan(context.Background(), &config.Plan{}, nil, nil); err == nil {
		t.Fatal("want error for nil env")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
