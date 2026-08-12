package report

// Bug 1 的 Red：实跑发现 test_total_time 输出完整 float64（如 0.0006185），
// Perl 端是 sprintf("%.2f", ...)（DisplayRole.pm:280）。当前实现
// Summary.TotalTime float64 直接 yaml.v3 序列化最短表示，本测试红。
// Green 需让 TotalTime 序列化时输出 2 位小数（字符串）。

import (
	"strings"
	"testing"
)

// totalTimeLine 返回输出里 `test_total_time:` 所在行原文，供精确断言。
func totalTimeLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "test_total_time:") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// TestRenderYAMLReport_TotalTime_TinyElapsed 验证极小 elapsed（0.0006185）
// 输出 2 位小数 0.00。
func TestRenderYAMLReport_TotalTime_TinyElapsed(t *testing.T) {
	summary := Summary{TotalNum: 1, Result: 1, Reason: "ok", TotalTime: 0.0006185}
	out, err := RenderYAMLReport(summary, nil, nil)
	if err != nil {
		t.Fatalf("RenderYAMLReport() error = %v", err)
	}
	line := totalTimeLine(string(out))
	if !(strings.Contains(line, `test_total_time: "0.00"`) || strings.Contains(line, `test_total_time: '0.00'`)) {
		t.Errorf("test_total_time 应为 2 位小数 0.00，实际行: %q\n输出:\n%s", line, out)
	}
}

// TestRenderYAMLReport_TotalTime_Seconds 验证正常时长（26.9000001）输出
// 26.90（2 位小数截断而非 26.9000001）。
func TestRenderYAMLReport_TotalTime_Seconds(t *testing.T) {
	summary := Summary{TotalNum: 1, Result: 1, Reason: "ok", TotalTime: 26.9000001}
	out, err := RenderYAMLReport(summary, nil, nil)
	if err != nil {
		t.Fatalf("RenderYAMLReport() error = %v", err)
	}
	line := totalTimeLine(string(out))
	if !(strings.Contains(line, `test_total_time: "26.90"`) || strings.Contains(line, `test_total_time: '26.90'`)) {
		t.Errorf("test_total_time 应为 26.90，实际行: %q\n输出:\n%s", line, out)
	}
}
