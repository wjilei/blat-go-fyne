package fyneui

import (
	"testing"

	"blat/internal/report"
)

func TestPlanResultLabel(t *testing.T) {
	cases := []struct {
		name      string
		sum       report.Summary
		cancelled bool
		wantText  string
		wantOK    bool
	}{
		{
			name:     "全部通过",
			sum:      report.Summary{TotalNum: 3, OKNum: 3, Result: "pass"},
			wantText: "成功",
			wantOK:   true,
		},
		{
			name:     "有失败",
			sum:      report.Summary{TotalNum: 3, OKNum: 2, FailNum: 1, Result: "fail"},
			wantText: "失败",
			wantOK:   false,
		},
		{
			name:      "用户取消",
			sum:       report.Summary{TotalNum: 3, OKNum: 1, FailNum: 0, Result: "pass"},
			cancelled: true,
			wantText:  "已取消",
			wantOK:    false,
		},
		{
			name:     "用户取消且原本有失败",
			sum:      report.Summary{TotalNum: 3, OKNum: 1, FailNum: 1, Result: "fail"},
			cancelled: true,
			wantText:  "已取消",
			wantOK:    false,
		},
		{
			name:     "总数为 0 也按通过处理（无失败即视为成功）",
			sum:      report.Summary{TotalNum: 0, OKNum: 0, Result: "pass"},
			wantText: "成功",
			wantOK:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotText, gotOK := planResultLabel(tc.sum, tc.cancelled)
			if gotText != tc.wantText {
				t.Errorf("text = %q, want %q", gotText, tc.wantText)
			}
			if gotOK != tc.wantOK {
				t.Errorf("success = %v, want %v", gotOK, tc.wantOK)
			}
		})
	}
}
