package report

// Phase 3 A 的红：CaseReport 需要 Extra map 承载 plan 自定义键（原 Args 改名
// Extra）。Phase 5（Bug 2 修复）把 `,inline` 弃用：yaml.v3 会把 inline 键
// 挤到 log/error 之后，顺序与 Perl DisplayRole 不符。现在 Extra 标 yaml:"-"
// 屏蔽直接序列化，平铺改由 RenderYAMLReport 的 mergeExtraIntoCase 完成。

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCaseReport_ExtraInline 验证 Extra 的键通过 RenderYAMLReport 平铺到 case
// 条目顶层，且 name/title/result/time 等保留字段不被吞掉（对齐 Perl
// DisplayRole.pm:69-88 把 plan args clone 进 case 条目的行为）。
func TestCaseReport_ExtraInline(t *testing.T) {
	cr := CaseReport{
		Seq:    1,
		Name:   "A::B",
		Title:  "hello",
		Result: CaseOK,
		Time:   0.5,
		Extra: map[string]any{
			"设备类型": "PSAV",
			"蓝牙操作": "read",
		},
	}
	data, err := RenderYAMLReport(Summary{TotalNum: 1, Result: 1, Reason: "ok", TotalTime: 0.5}, nil, []CaseReport{cr})
	if err != nil {
		t.Fatalf("RenderYAMLReport: %v", err)
	}
	out := string(data)

	// 自定义键必须平铺到 case 条目顶层（而不是嵌套在 args: 下面）。
	for _, want := range []string{"设备类型: PSAV", "蓝牙操作: read"} {
		if !strings.Contains(out, want) {
			t.Errorf("Extra 键 %q 未平铺到顶层:\n%s", want, out)
		}
	}
	// 保留字段必须仍在顶层（merge 不应吞掉 reserved 字段）。
	for _, want := range []string{
		"case_seq: 1",
		"name: A::B",
		"title: hello",
		"result: ok",
		"time: 0.5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("保留字段 %q 缺失（merge 污染）:\n%s", want, out)
		}
	}
}

// TestCaseReport_ExtraOmitEmpty 验证 Extra 为空（nil/空 map）时该字段整段
// 省略，报告里不会出现裸的 `extra:` 或残留的旧 `args:`。
func TestCaseReport_ExtraOmitEmpty(t *testing.T) {
	cr := CaseReport{Seq: 1, Name: "A::B", Result: CaseOK}
	data, err := yaml.Marshal(cr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)
	for _, banned := range []string{"extra:", "Extra:", "args:"} {
		if strings.Contains(out, banned) {
			t.Errorf("空 Extra 不应输出 %q:\n%s", banned, out)
		}
	}
}
