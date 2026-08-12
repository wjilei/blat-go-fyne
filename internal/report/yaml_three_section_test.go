package report

// Phase 1 Red：让 YAMLReporter 输出对齐 Perl ConfigRole yaml_dump($summary,
// $env, $reports) 的三段式。
//
// 三段顺序严格：summary 块（第 1 个 `---` 之后）→ env 块（中间 `---`）→ cases
// 数组（末尾）。env 段是从 Env.Vars clone 来的全量透出；唯一过滤是遍历一层
// hash 把 key 为 `password` 的值替换成 `******`，与 Perl ConfigRole.pm:1028
// 保持一致。
//
// 这些测试当前会因 YAMLReporter 不输出三段式而失败；Phase 1 Green 阶段
// 需要新增 `WithVars` setter 与相应序列化逻辑才能让它们过。

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestYAMLReporter_ThreeSectionLayout 锁定报告三段顺序：summary → env → cases
// 在 YAML 字节流里的相对位置。
func TestYAMLReporter_ThreeSectionLayout(t *testing.T) {
	var buf bytes.Buffer
	r := NewYAML(&buf).WithVars(map[string]any{
		"HeatNote": map[string]any{
			"lot":     "W262813",
			"mac":     "262602460054",
			"产品类型": "PSAv2", // 中文 key 用于确认 Unicode 透出
		},
		"SERIAL": map[string]any{
			"serial": "COM7",
		},
	})

	r.OnPlanStart(2, time.Now())
	r.OnCaseStop(1, CaseReport{
		Seq:    1,
		Name:   "PSAV::config_check",
		Title:  "检查参数",
		Result: CaseOK,
		Time:   0.7,
	})
	r.OnCaseStop(2, CaseReport{
		Seq:    2,
		Name:   "PSAV::read_params",
		Title:  "读取参数",
		Result: CaseFail,
		Time:   1.2,
		Error:  "boom",
	})
	r.OnPlanStop(Summary{
		TotalNum:   2,
		PlanedNum:  2,
		RunningNum: 0,
		OKNum:      1,
		FailNum:    1,
		Result:     0,
		Reason:     "读取参数",
		TotalTime:  26.90,
	})

	out := buf.String()

	// summary 段必须出现 8 个 test_* 键。
	for _, want := range []string{
		"test_total_num: 2",
		"test_planed_num: 2",
		"test_runing_num: 0",
		"test_ok_num: 1",
		"test_fail_num: 1",
		"test_result: 0",
		"test_failreason: 读取参数",
		"test_total_time: \"26.90\"", // Bug 1 修复：TotalTime 以 %.2f 字符串输出
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q in:\n%s", want, out)
		}
	}

	// env 段透出 HeatNote 全量，包括中文 key。YAML 库对纯数字字符串会
	// 加引号（如 "262602460054"），断言只用关键字面值对比，不绑死 YAML 词法。
	for _, want := range []string{
		"HeatNote:",
		"262602460054",                      // mac，可能是 "262602460054"
		"W262813",                            // lot 短，无需引号
		"产品类型: PSAv2",                    // 中文 key + 字母值，无引号
		"SERIAL:",
		"serial: COM7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("env section missing %q in:\n%s", want, out)
		}
	}

	// 顺序断言：summary 关键字位置 < env 关键字位置 < 第一个 case name 位置。
	posSummary := strings.Index(out, "test_total_num:")
	posEnv := strings.Index(out, "HeatNote:")
	posCase := strings.Index(out, "name: PSAV::config_check")
	if posSummary < 0 || posEnv < 0 || posCase < 0 {
		t.Fatalf("missing one of summary/env/case markers in:\n%s", out)
	}
	if !(posSummary < posEnv && posEnv < posCase) {
		t.Errorf("三段顺序错误：summary=%d env=%d case=%d\n输出:\n%s",
			posSummary, posEnv, posCase, out)
	}

	// cases 段含两个 case_seq
	if strings.Count(out, "case_seq:") != 2 {
		t.Errorf("expected 2 case_seq, got %d in:\n%s",
			strings.Count(out, "case_seq:"), out)
	}
	if !strings.Contains(out, "error: boom") {
		t.Errorf("case failure should carry error field in:\n%s", out)
	}
}

// TestYAMLReporter_PasswordMasked 验证 env 段中嵌套 map 的 password 字段被
// 打码为 `******`；其余字段原样保留。
func TestYAMLReporter_PasswordMasked(t *testing.T) {
	var buf bytes.Buffer
	vars := map[string]any{
		"OAuth": map[string]any{
			"client_id":     "abc",
			"password":      "supersecret-123", // 应被替换
			"refresh_token": "tok-xyz",
		},
		"plain_password": "top-level-scalar", // 顶层 string 不会被 Cloning 当作 hash key 处理
	}
	r := NewYAML(&buf).WithVars(vars)

	r.OnPlanStart(1, time.Now())
	r.OnCaseStop(1, CaseReport{Seq: 1, Name: "A::B", Result: CaseOK})
	r.OnPlanStop(Summary{TotalNum: 1, OKNum: 1, Result: 1, Reason: "ok", TotalTime: 1.23})

	out := buf.String()

	// 嵌套 password 应被打码。
	if !strings.Contains(out, "password: '******'") && !strings.Contains(out, "password: \"******\"") {
		t.Errorf("嵌套 map 的 password 字段应被打码为 ******，但未发现。\n输出:\n%s", out)
	}
	// 原值不应泄漏。
	if strings.Contains(out, "supersecret-123") {
		t.Errorf("password 原值泄露在输出中:\n%s", out)
	}
	// 同级其他字段原样保留。
	for _, want := range []string{
		"client_id: abc",
		"refresh_token: tok-xyz",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("OAuth 段缺字段 %q in:\n%s", want, out)
		}
	}
}
