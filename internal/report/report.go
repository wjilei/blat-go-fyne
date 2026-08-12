// Package report defines the Reporter interface used by the plan runner
// to emit test progress and results in one or more formats.
//
// The interface mirrors the Perl Runner's DisplayRole state machine:
// a plan goes through OnPlanStart / per-attempt OnCaseStart + OnCaseStop /
// OnPlanStop. A Reporter is free to buffer events (YAML) or stream them
// (TAP-13). MultiReporter fans a single event stream out to any number of
// concrete reporters, so the console, files and (later) the Fyne UI can
// all subscribe to the same run.
package report

import (
	"fmt"
	"time"
)

// CaseResult mirrors the Perl DisplayRole result states.
type CaseResult string

const (
	CasePlanned CaseResult = "planned"
	CaseRunning CaseResult = "running"
	CaseOK      CaseResult = "ok"
	CaseFail    CaseResult = "fail"
)

// CaseReport is the per-attempt result passed to OnCaseStart/OnCaseStop.
// YAML tags match the case entries produced by the Perl report writer.
type CaseReport struct {
	Seq    int            `yaml:"case_seq"`
	Name   string         `yaml:"name"`
	Title  string         `yaml:"title,omitempty"`
	Result CaseResult     `yaml:"result"`
	Time   float64        `yaml:"time,omitempty"` // seconds
	// Log 是该 case 窗口内的原始日志全文（含 case_start ... case_stop 行）。
	// Phase 2 C：从 []string 改为 string，yaml.v3 渲染为 `|` 块字符串，
	// 对齐 Perl DisplayRole.pm:153-195 把日志按 case_seq 归并到
	// $report->{log} 的多行标量语义。
	Log   string `yaml:"log,omitempty"`
	// Extra 承载 plan 中除保留字段外的全部自定义键。`,inline` 已弃用：yaml.v3
	// 会把 inline 键展开到条目末尾（log/error 之后），与 Perl DisplayRole 的
	// 目标顺序不符。改为 yaml:"-" 屏蔽直接序列化，由 RenderYAMLReport 的
	// mergeExtraIntoCase 按 case_seq/name/title/result/time/[desc/][args]/log/
	// [error] 手工合并进 case 顶层 map（Bug 2 修复）。
	Extra map[string]any `yaml:"-"`
	Error string         `yaml:"error,omitempty"`
}

// MarshalYAML 让 yaml.v3 序列化 Summary 时把 TotalTime 格式化为 2 位小数字符
// 串（仿 Perl DisplayRole.pm:280 sprintf("%.2f", time() - test_start_time)）。
// 返回匿名 struct 而非 map：struct 按字段声明顺序输出，避免 map 键字典序
// 重排破坏 summary 段的键顺序。
func (s Summary) MarshalYAML() (interface{}, error) {
	type summaryAlias struct {
		TotalNum   int    `yaml:"test_total_num"`
		PlanedNum  int    `yaml:"test_planed_num"`
		RunningNum int    `yaml:"test_runing_num"`
		OKNum      int    `yaml:"test_ok_num"`
		FailNum    int    `yaml:"test_fail_num"`
		Result     int    `yaml:"test_result"`
		Reason     string `yaml:"test_failreason"`
		TotalTime  string `yaml:"test_total_time"`
		StartTime  string `yaml:"start_time,omitempty"`
		StopTime   string `yaml:"stop_time,omitempty"`
	}
	return summaryAlias{
		TotalNum:   s.TotalNum,
		PlanedNum:  s.PlanedNum,
		RunningNum: s.RunningNum,
		OKNum:      s.OKNum,
		FailNum:    s.FailNum,
		Result:     s.Result,
		Reason:     s.Reason,
		TotalTime:  fmt.Sprintf("%.2f", s.TotalTime),
		StartTime:  s.StartTime,
		StopTime:   s.StopTime,
	}, nil
}

// Summary is the plan-level result passed to OnPlanStop.
//
// 字段名严格对齐 Perl BLAT 的 report.yml：8 个 test_* 键 + 顶层 start_time/stop_time。
// Phase 1 pre-red 同步阶段把 Result 从 string 改为 int（1 成功 / 0 失败，与 Perl
// ConfigRole 一致）；Reason 成功时固定为 "ok"（DisplayRole app_summary 默认）。
type Summary struct {
	TotalNum   int     `yaml:"test_total_num"`
	PlanedNum  int     `yaml:"test_planed_num"`
	RunningNum int     `yaml:"test_runing_num"`
	OKNum      int     `yaml:"test_ok_num"`
	FailNum    int     `yaml:"test_fail_num"`
	Result     int     `yaml:"test_result"` // 1 pass / 0 fail；Perl 端是 0/1 数字而非字符串。
	Reason     string  `yaml:"test_failreason"` // 全程存在；成功必为 "ok"，失败时是首个失败用例 title。
	TotalTime  float64 `yaml:"test_total_time"`  // Wall-clock 秒数（2 位小数）；Perl 端 sprintf("%.2f", ...)。
	StartTime  string  `yaml:"start_time,omitempty"`
	StopTime   string  `yaml:"stop_time,omitempty"`
}

// Reporter receives plan lifecycle events. Implementations must be safe to
// call from the runner goroutine. A nil Reporter is tolerated by the
// runner and simply skips every call.
type Reporter interface {
	OnPlanStart(total int, startTime time.Time)
	OnCaseStart(seq int, cr CaseReport)
	OnCaseStop(seq int, cr CaseReport)
	OnPlanStop(summary Summary)
}
