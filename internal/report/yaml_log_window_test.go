package report

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"blat/internal/logfile"
)

// TestYAMLReporter_LogWindow 验证 YAMLReporter 接 logfile 后按 case 窗口
// 切片日志：OnCaseStart 记录 (offset, gen)，OnCaseStop 用 TailFrom 拿到该
// case 期间新增的行写入 cr.Log（块字符串）。对齐 Perl DisplayRole.pm:153-195
// 按 case_seq 切片日志归并到 $report->{log} 的行为。
//
// Phase 2 C 的红：当前 YAMLReporter 无 WithLogfile，Log 字段为 []string，
// 测试无法编译。
func TestYAMLReporter_LogWindow(t *testing.T) {
	dir := t.TempDir()
	lf, err := logfile.Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("logfile.Open: %v", err)
	}
	defer lf.Close()

	var buf bytes.Buffer
	y := NewYAML(&buf).WithLogfile(lf)

	y.OnPlanStart(1, time.Now())

	// 本 case 之前已存在的日志（如上一个 case 的尾巴）：不应混入窗口。
	if err := lf.WriteLine("info", "RUNNER", "case_stop PrevCase ok 0.10"); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}

	y.OnCaseStart(1, CaseReport{Seq: 1, Name: "A::B", Result: CaseRunning})

	// case 运行期间写入的日志（case_start ... case_stop 全量 raw log）。
	for _, line := range []string{
		"case_start A::B",
		"读取参数 ok",
		"电压: 3.60",
		"case_stop A::B ok 0.25",
	} {
		if err := lf.WriteLine("info", "RUNNER", line); err != nil {
			t.Fatalf("WriteLine: %v", err)
		}
	}

	y.OnCaseStop(1, CaseReport{Seq: 1, Name: "A::B", Result: CaseOK, Time: 0.25})
	y.OnPlanStop(Summary{TotalNum: 1, OKNum: 1, Result: 1, Reason: "ok", TotalTime: 0.3})

	out := buf.String()

	// window 内的全部行必须出现在 YAML 中（块字符串字面内容）。
	for _, want := range []string{
		"case_start A::B",
		"读取参数 ok",
		"电压: 3.60",
		"case_stop A::B ok 0.25",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("YAML 输出缺少 window 内日志行 %q in:\n%s", want, out)
		}
	}
	// 窗口外（case 之前的日志）不得混入。
	if strings.Contains(out, "PrevCase") {
		t.Errorf("窗口外日志混入 case 的 log 字段:\n%s", out)
	}
	// log 应为 | 块字符串（对齐 Perl yaml_dump 多行标量）。
	if !strings.Contains(out, "log: |") {
		t.Errorf("log 字段应为 | 块字符串，实际输出:\n%s", out)
	}
}
