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

import "time"

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
	Log    []string       `yaml:"log,omitempty"`
	Args   map[string]any `yaml:"args,omitempty"`
	Error  string         `yaml:"error,omitempty"`
}

// Summary is the plan-level result passed to OnPlanStop.
type Summary struct {
	TotalNum   int     `yaml:"test_total_num"`
	PlanedNum  int     `yaml:"test_planed_num"`
	RunningNum int     `yaml:"test_runing_num"`
	OKNum      int     `yaml:"test_ok_num"`
	FailNum    int     `yaml:"test_fail_num"`
	Result     string  `yaml:"test_result"` // "pass" / "fail"
	Reason     string  `yaml:"test_failreason,omitempty"`
	StartTime  string  `yaml:"start_time,omitempty"`
	StopTime   string  `yaml:"stop_time,omitempty"`
	Duration   float64 `yaml:"duration,omitempty"`
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
