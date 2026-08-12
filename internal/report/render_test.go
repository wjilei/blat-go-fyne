package report

// Phase 4 A Red：RenderYAMLReport 是给 uploader 在 OnPlanStop 阶段复用三段式
// 序列化的纯函数入口（对齐 Perl DisplayRole.pm:281-286 的
// $self->save_report_file({format => 'yml', tostr => \$log_str})）。它不写文件、
// 不接 logfile，只返回字节流，让 uploader 无需 new YAMLReporter 就能拿到
// summary → env(vars) → cases 三段 YAML。

import (
	"strings"
	"testing"
)

// TestRenderYAMLReport_ThreeSections 锁定 RenderYAMLReport 输出包含三段：
// summary（8 个 test_* 键）→ env vars（嵌套 password 打码）→ cases（两个
// case_seq，失败项含 error + result: fail），三个 `---` 分隔符。
func TestRenderYAMLReport_ThreeSections(t *testing.T) {
	summary := Summary{
		TotalNum:   2,
		PlanedNum:  2,
		RunningNum: 0,
		OKNum:      1,
		FailNum:    1,
		Result:     0,
		Reason:     "读取参数",
		TotalTime:  26.90,
	}
	vars := map[string]any{
		"HeatNote": map[string]any{
			"lot":     "W262813",
			"mac":     "262602460054",
			"产品类型": "PSAv2",
		},
		"OAuth": map[string]any{
			"client_id": "abc",
			"password":  "supersecret-123", // 必须被打码
		},
	}
	cases := []CaseReport{
		{
			Seq:    1,
			Name:   "PSAV::config_check",
			Title:  "检查参数",
			Result: CaseOK,
			Time:   0.7,
			Extra:  map[string]any{"param1": "x"},
		},
		{
			Seq:    2,
			Name:   "PSAV::read_params",
			Title:  "读取参数",
			Result: CaseFail,
			Time:   1.2,
			Error:  "boom",
		},
	}

	out, err := RenderYAMLReport(summary, vars, cases)
	if err != nil {
		t.Fatalf("RenderYAMLReport() error = %v", err)
	}
	s := string(out)

	// 三个 `---` 分隔符（summary / vars / cases 各段前一个）。
	if got := strings.Count(s, "---"); got != 3 {
		t.Errorf("`---` 分隔符数量 = %d, want 3\n输出:\n%s", got, s)
	}

	// 段 1：summary 块含 8 个 test_* 键。
	for _, want := range []string{
		"summary:",
		"test_total_num: 2",
		"test_planed_num: 2",
		"test_runing_num: 0",
		"test_ok_num: 1",
		"test_fail_num: 1",
		"test_result: 0",
		"test_failreason: 读取参数",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary 段缺 %q in:\n%s", want, s)
		}
	}

	// 段 2：env vars 透出，嵌套 password 打码为 ******，原值不泄漏。
	if !strings.Contains(s, "password: '******'") && !strings.Contains(s, "password: \"******\"") {
		t.Errorf("嵌套 password 应被打码为 ******，未发现。\n输出:\n%s", s)
	}
	if strings.Contains(s, "supersecret-123") {
		t.Errorf("password 原值泄露在输出中:\n%s", s)
	}
	for _, want := range []string{"HeatNote:", "lot: W262813", "client_id: abc"} {
		if !strings.Contains(s, want) {
			t.Errorf("env 段缺 %q in:\n%s", want, s)
		}
	}

	// 段 3：cases 序列，两个 case_seq；case2 含 error + result: fail。
	if got := strings.Count(s, "case_seq:"); got != 2 {
		t.Errorf("case_seq 数量 = %d, want 2\n输出:\n%s", got, s)
	}
	if !strings.Contains(s, "error: boom") {
		t.Errorf("case2 应含 error 字段 in:\n%s", s)
	}
	if !strings.Contains(s, "result: fail") {
		t.Errorf("case2 应含 result: fail in:\n%s", s)
	}
	if !strings.Contains(s, "result: ok") {
		t.Errorf("case1 应含 result: ok in:\n%s", s)
	}

	// 三段顺序：summary < env < cases。
	posSummary := strings.Index(s, "test_total_num:")
	posEnv := strings.Index(s, "HeatNote:")
	posCase := strings.Index(s, "name: PSAV::config_check")
	if posSummary < 0 || posEnv < 0 || posCase < 0 {
		t.Fatalf("缺少 summary/env/case 标记:\n%s", s)
	}
	if !(posSummary < posEnv && posEnv < posCase) {
		t.Errorf("三段顺序错误：summary=%d env=%d case=%d\n输出:\n%s",
			posSummary, posEnv, posCase, s)
	}
}

// TestRenderYAMLReport_NoCases 验证 cases 为空时第三段省略，输出只有
// summary + vars 两段（2 个 `---`）。
func TestRenderYAMLReport_NoCases(t *testing.T) {
	summary := Summary{TotalNum: 0, Result: 1, Reason: "ok", TotalTime: 1.23}
	out, err := RenderYAMLReport(summary, map[string]any{"a": "b"}, nil)
	if err != nil {
		t.Fatalf("RenderYAMLReport() error = %v", err)
	}
	s := string(out)
	if got := strings.Count(s, "---"); got != 2 {
		t.Errorf("`---` 分隔符数量 = %d, want 2\n输出:\n%s", got, s)
	}
	if !strings.Contains(s, "test_total_num: 0") {
		t.Errorf("summary 段缺 test_total_num in:\n%s", s)
	}
}
