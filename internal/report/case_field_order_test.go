package report

// Bug 2 的 Red：实跑发现 case 条目字段顺序错位——Error 被 `,inline` 的 Extra
// 键挤到 log 之后（当前声明顺序 Seq/Name/Title/Result/Time/Log/Extra/Error，
// yaml.v3 把 inline 展开键放在最后）。目标顺序对齐 Perl DisplayRole.pm:
// case_seq/name/title/result/time/[desc/][args]/log/[error]。
//
// 当前实现下本测试红：yaml.v3 输出 `error` 在 log 之后、Extra 键（设备类型）
// 在 error 之后，`desc` 位置也不受控。Green 需把 Extra 改为 yaml:"-" 并在
// RenderYAMLReport 里手工按目标顺序合并 case 条目。

import (
	"strings"
	"testing"
)

// TestRenderYAMLReport_CaseFieldOrder 锁定单个失败 case 的顶层键顺序：
// case_seq < name < title < result < time < desc < Extra键 < log < error。
// desc != title 时必须保留，且紧跟 time（在 args 之前）。
func TestRenderYAMLReport_CaseFieldOrder(t *testing.T) {
	summary := Summary{TotalNum: 1, Result: 0, Reason: "boom", TotalTime: 0.5}
	cases := []CaseReport{
		{
			Seq:    1,
			Name:   "X",
			Title:  "hello",
			Result: CaseFail,
			Time:   0.5,
			Log:    "log-line\n",
			Error:  "boom",
			Extra:  map[string]any{"设备类型": "PSAV", "desc": "desc-not-equal-title"},
		},
	}

	out, err := RenderYAMLReport(summary, nil, cases)
	if err != nil {
		t.Fatalf("RenderYAMLReport() error = %v", err)
	}
	s := string(out)

	order := []string{
		"case_seq: 1",
		"name: X",
		"title: hello",
		"result: fail",
		"time: 0.5",
		"desc: desc-not-equal-title",
		"设备类型: PSAV",
		"log: |",
		"error: boom",
	}
	pos := 0
	for _, want := range order {
		p := strings.Index(s[pos:], want)
		if p < 0 {
			t.Fatalf("字段顺序断言：从位置 %d 起找不到 %q\n输出:\n%s", pos, want, s)
		}
		pos += p + len(want)
	}
}

// TestRenderYAMLReport_CaseFieldOrder_DescEqualsTitle 验证 desc == title 时
// desc 被剔除（DisplayRole.pm:69-88 删冗余规则），且剩余 Extra 键仍在 log
// 之前、error 之后。
func TestRenderYAMLReport_CaseFieldOrder_DescEqualsTitle(t *testing.T) {
	summary := Summary{TotalNum: 1, Result: 1, Reason: "ok", TotalTime: 0.5}
	cases := []CaseReport{
		{
			Seq:    1,
			Name:   "X",
			Title:  "hello",
			Result: CaseOK,
			Time:   0.1,
			Log:    "log-line\n",
			Error:  "boom",
			Extra:  map[string]any{"蓝牙操作": "read", "desc": "hello"},
		},
	}

	out, err := RenderYAMLReport(summary, nil, cases)
	if err != nil {
		t.Fatalf("RenderYAMLReport() error = %v", err)
	}
	s := string(out)

	if strings.Contains(s, "desc: hello") {
		t.Errorf("desc == title 时不应输出 desc:\n%s", s)
	}
	posExtra := strings.Index(s, "蓝牙操作: read")
	posLog := strings.Index(s, "log: |")
	posError := strings.Index(s, "error: boom")
	if posExtra < 0 || posLog < 0 || posError < 0 {
		t.Fatalf("缺少 Extra/log/error 标记:\n%s", s)
	}
	if !(posExtra < posLog && posLog < posError) {
		t.Errorf("顺序错误：Extra=%d log=%d error=%d，want Extra < log < error\n输出:\n%s",
			posExtra, posLog, posError, s)
	}
}
