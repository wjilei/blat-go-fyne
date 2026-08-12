package runtime

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"blat/internal/config"
	"blat/internal/core"
)

// logRow 记录一次 Logger 调用的 (category, msg)。
type logRow struct {
	category string
	msg      string
}

// logRecorder 记录所有 Logger 调用，供断言 Runner 的 RUNNER category 日志。
type logRecorder struct {
	mu   sync.Mutex
	rows []logRow
}

func (l *logRecorder) Info(category, msg string)  { l.add(category, msg) }
func (l *logRecorder) Warn(category, msg string)  { l.add(category, msg) }
func (l *logRecorder) Error(category, msg string) { l.add(category, msg) }

func (l *logRecorder) add(category, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, logRow{category, msg})
}

func (l *logRecorder) rowsSnapshot() []logRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logRow, len(l.rows))
	copy(out, l.rows)
	return out
}

// caseStopRe 匹配 case_stop <name> <ok|fail> <time>，time 为两位小数。
func caseStopRe(name, result string) *regexp.Regexp {
	return regexp.MustCompile(`^case_stop ` + regexp.QuoteMeta(name) + ` ` + result + ` \d+\.\d{2}$`)
}

// TestRunPlan_RunnerCategoryLog 验证 PlanRunner 在每个 case 前后写
// RUNNER category 的 case_start / case_stop 行（仿 Perl Runner.pm:119/161，
// Log::Any category=RUNNER）。这是 Phase 2 B 的红：当前 runner 不写这两行。
func TestRunPlan_RunnerCategoryLog(t *testing.T) {
	reg := NewRegistry()
	reg.Register("HelloSuite::SayHello", func() (core.Case, error) {
		return &fakeCase{name: "SayHello"}, nil
	})
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{
		{Name: "HelloSuite::SayHello", Title: "a", Counts: 2},
	}}
	log := &logRecorder{}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}, Log: log}

	if err := pr.RunPlan(context.Background(), plan, env, nil); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	rows := log.rowsSnapshot()
	// 2 次尝试 × (case_start + case_stop) = 4 行
	if len(rows) != 4 {
		t.Fatalf("日志行数 = %d, want 4 (2 次尝试 × start+stop):\n%+v", len(rows), rows)
	}
	for i := 0; i < 2; i++ {
		start := rows[i*2]
		stop := rows[i*2+1]
		if start.category != "RUNNER" {
			t.Errorf("case_start[%d] category = %q, want RUNNER", i, start.category)
		}
		if !strings.HasPrefix(start.msg, "case_start HelloSuite::SayHello") {
			t.Errorf("case_start[%d] msg = %q, want 前缀 case_start HelloSuite::SayHello", i, start.msg)
		}
		if stop.category != "RUNNER" {
			t.Errorf("case_stop[%d] category = %q, want RUNNER", i, stop.category)
		}
		if !caseStopRe("HelloSuite::SayHello", "ok").MatchString(stop.msg) {
			t.Errorf("case_stop[%d] msg = %q, want 匹配 ^case_stop HelloSuite::SayHello ok \\d+\\.\\d{2}$", i, stop.msg)
		}
	}
}

// TestRunPlan_RunnerCategoryLog_Fail 验证失败 case 的 case_stop 写 fail。
func TestRunPlan_RunnerCategoryLog_Fail(t *testing.T) {
	reg := NewRegistry()
	reg.Register("F::C", func() (core.Case, error) {
		return &fakeCase{name: "C", err: errors.New("boom")}, nil
	})
	pr := NewPlanRunner(reg)
	plan := &config.Plan{Cases: []config.CaseItem{{Name: "F::C", Counts: 1}}}
	log := &logRecorder{}
	env := &core.Env{Ctx: context.Background(), Vars: map[string]any{}, Log: log}

	if err := pr.RunPlan(context.Background(), plan, env, nil); err == nil {
		t.Fatal("want error from failing case")
	}

	rows := log.rowsSnapshot()
	if len(rows) != 2 {
		t.Fatalf("日志行数 = %d, want 2:\n%+v", len(rows), rows)
	}
	if rows[0].category != "RUNNER" || !strings.HasPrefix(rows[0].msg, "case_start F::C") {
		t.Errorf("case_start = %+v, want RUNNER case_start F::C", rows[0])
	}
	if rows[1].category != "RUNNER" || !caseStopRe("F::C", "fail").MatchString(rows[1].msg) {
		t.Errorf("case_stop = %+v, want RUNNER ^case_stop F::C fail \\d+\\.\\d{2}$", rows[1])
	}
}
