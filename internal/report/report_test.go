package report

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestYAMLReporter(t *testing.T) {
	var buf bytes.Buffer
	r := NewYAML(&buf)
	r.OnPlanStart(2, time.Now())
	r.OnCaseStop(1, CaseReport{
		Seq: 1, Name: "HelloSuite::SayHello", Title: "招呼",
		Result: CaseOK, Time: 0.001,
	})
	r.OnCaseStop(2, CaseReport{
		Seq: 2, Name: "HelloSuite::SayHello", Title: "招呼",
		Result: CaseFail, Time: 0.002, Error: "boom",
	})
	r.OnPlanStop(Summary{TotalNum: 2, PlanedNum: 2, RunningNum: 2, OKNum: 1, FailNum: 1, Result: "fail"})

	out := buf.String()
	for _, want := range []string{
		"case_seq: 1",
		"name: HelloSuite::SayHello",
		"title: 招呼",
		"result: ok",
		"time: 0.001",
		"case_seq: 2",
		"result: fail",
		"error: boom",
		"summary:",
		"test_fail_num: 1",
		"test_result: fail",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("YAML output missing %q in:\n%s", want, out)
		}
	}
}

func TestYAMLReporter_FileMode(t *testing.T) {
	dir := t.TempDir()
	r := NewYAMLFile(dir)
	r.OnPlanStart(1, time.Now())
	r.OnCaseStop(1, CaseReport{Seq: 1, Name: "A::B", Result: CaseOK})
	r.OnPlanStop(Summary{TotalNum: 1, OKNum: 1, Result: "pass"})

	matches, err := filepath.Glob(filepath.Join(dir, "report_*.yml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one report file, got %d", len(matches))
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	if !strings.Contains(string(data), "case_seq: 1") {
		t.Errorf("report file missing case entry:\n%s", data)
	}
}

// TestYAMLReporter_PathMode verifies the fixed-path mode truncates any
// previous file at OnPlanStart so each run starts with a clean slate.
func TestYAMLReporter_PathMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.yml")

	// 第一次 run：写入一份完整报告。
	r1 := NewYAMLPath(path)
	r1.OnPlanStart(1, time.Now())
	r1.OnCaseStop(1, CaseReport{Seq: 1, Name: "A::B", Result: CaseOK})
	r1.OnPlanStop(Summary{TotalNum: 1, OKNum: 1, Result: "pass"})

	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after first run: %v", err)
	}
	if !strings.Contains(string(first), "case_seq: 1") {
		t.Fatalf("first run missing case entry:\n%s", first)
	}

	// 第二次 run：覆盖写到同一路径，case 数量也不同，验证旧内容被截断。
	r2 := NewYAMLPath(path)
	r2.OnPlanStart(2, time.Now())
	r2.OnCaseStop(1, CaseReport{Seq: 1, Name: "X::Y", Result: CaseOK})
	r2.OnCaseStop(2, CaseReport{Seq: 2, Name: "X::Z", Result: CaseOK})
	r2.OnPlanStop(Summary{TotalNum: 2, OKNum: 2, Result: "pass"})

	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}
	s := string(second)
	if strings.Contains(s, "A::B") {
		t.Errorf("second run should not contain first run's case name; got:\n%s", s)
	}
	if !strings.Contains(s, "X::Y") || !strings.Contains(s, "X::Z") {
		t.Errorf("second run missing new case entries:\n%s", s)
	}
}

func TestTAPReporter(t *testing.T) {
	var buf bytes.Buffer
	r := NewTAP(&buf)
	r.OnPlanStart(3, time.Now())
	r.OnCaseStop(1, CaseReport{Seq: 1, Name: "HelloSuite::SayHello", Title: "SayHello", Result: CaseOK})
	r.OnCaseStop(2, CaseReport{Seq: 2, Name: "HelloSuite::SayHello", Title: "SayHello", Result: CaseOK})
	r.OnCaseStop(3, CaseReport{Seq: 3, Name: "SomeCase", Result: CaseFail, Error: "expected X, got Y"})
	r.OnPlanStop(Summary{FailNum: 1, Result: "fail"})

	want := `TAP version 13
1..3
ok 1 - SayHello
ok 2 - SayHello
not ok 3 - SomeCase
  ---
  message: 'expected X, got Y'
  ...
Bail out! 1 case(s) failed
`
	if buf.String() != want {
		t.Errorf("TAP output mismatch.\ngot:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestTAPReporter_PassOnly(t *testing.T) {
	var buf bytes.Buffer
	r := NewTAP(&buf)
	r.OnPlanStart(1, time.Now())
	r.OnCaseStop(1, CaseReport{Seq: 1, Name: "A::B", Result: CaseOK})
	r.OnPlanStop(Summary{OKNum: 1, Result: "pass"})

	if strings.Contains(buf.String(), "Bail out!") {
		t.Errorf("unexpected Bail out for a passing plan:\n%s", buf.String())
	}
}

// capture records every reporter event it receives.
type capture struct {
	planStarts int
	caseStarts []int
	caseStops  []int
	planStops  int
}

func (c *capture) OnPlanStart(total int, startTime time.Time) { c.planStarts++ }
func (c *capture) OnCaseStart(seq int, cr CaseReport)         { c.caseStarts = append(c.caseStarts, seq) }
func (c *capture) OnCaseStop(seq int, cr CaseReport)          { c.caseStops = append(c.caseStops, seq) }
func (c *capture) OnPlanStop(summary Summary)                 { c.planStops++ }

func TestMultiReporter(t *testing.T) {
	subs := []*capture{{}, {}, {}}
	rs := make([]Reporter, len(subs))
	for i, s := range subs {
		rs[i] = s
	}
	m := NewMulti(rs...)
	m.OnPlanStart(2, time.Now())
	m.OnCaseStart(1, CaseReport{Seq: 1, Result: CaseRunning})
	m.OnCaseStop(1, CaseReport{Seq: 1, Result: CaseOK})
	m.OnPlanStop(Summary{Result: "pass"})

	for i, s := range subs {
		if s.planStarts != 1 {
			t.Errorf("sub %d: planStarts = %d, want 1", i, s.planStarts)
		}
		if len(s.caseStarts) != 1 || s.caseStarts[0] != 1 {
			t.Errorf("sub %d: caseStarts = %v, want [1]", i, s.caseStarts)
		}
		if len(s.caseStops) != 1 || s.caseStops[0] != 1 {
			t.Errorf("sub %d: caseStops = %v, want [1]", i, s.caseStops)
		}
		if s.planStops != 1 {
			t.Errorf("sub %d: planStops = %d, want 1", i, s.planStops)
		}
	}
}
