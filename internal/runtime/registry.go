// Package runtime wires YAML plans to Go Case implementations.
//
// The Perl port relies on Module::Load + 反射方法名 (e.g. Cases::Foo->bar)
// to dispatch cases. The Go side keeps the same `<Suite>::<Method>` naming
// convention but uses an explicit registry to avoid heavy reflection. A
// case author registers a factory by name; the YAML plan just references
// that name.
package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/report"

	"gopkg.in/yaml.v3"
)

// Factory builds a fresh Case instance for one execution.
type Factory func() (core.Case, error)

// Registry maps a YAML case name to a Factory.
type Registry struct {
	cases map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{cases: map[string]Factory{}}
}

// Register associates a name with a Factory. Re-registration overwrites
// the previous entry; this is intentional so test code can rebind cases.
func (r *Registry) Register(name string, f Factory) {
	r.cases[name] = f
}

// Names returns the registered case names in unspecified order. Useful
// for diagnostics.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.cases))
	for k := range r.cases {
		out = append(out, k)
	}
	return out
}

// Invoke builds a Case for the given plan name.
func (r *Registry) Invoke(name string) (core.Case, error) {
	f, ok := r.cases[name]
	if !ok {
		return nil, fmt.Errorf("case not registered: %s", name)
	}
	c, err := f()
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", name, err)
	}
	return c, nil
}

// PlanRunner executes a parsed Plan against a Registry. It is the Go
// counterpart of the Perl Runner's plan-driven mode.
type PlanRunner struct {
	Reg *Registry
}

func NewPlanRunner(reg *Registry) *PlanRunner {
	return &PlanRunner{Reg: reg}
}

// RunPlan walks the plan in order and returns the first failure. Each case
// is attempted Counts times. If rep is non-nil it receives one OnPlanStart
// before the first attempt, one OnCaseStart/OnCaseStop pair per attempt
// (seq is the 1-based test number across the whole plan), and one
// OnPlanStop at the end — including the failure paths. Pass nil to skip
// all reporting.
func (p *PlanRunner) RunPlan(
	ctx context.Context,
	plan *config.Plan,
	env *core.Env,
	rep report.Reporter,
) error {
	if plan == nil {
		return fmt.Errorf("nil plan")
	}
	if env == nil {
		return fmt.Errorf("nil env")
	}
	total := 0
	for _, it := range plan.Cases {
		total += it.Counts
	}
	start := time.Now()
	if rep != nil {
		rep.OnPlanStart(total, start)
	}
	okNum, failNum := 0, 0
	finish := func(reason string) {
		if rep == nil {
			return
		}
		rep.OnPlanStop(buildSummary(total, okNum, failNum, start, reason))
	}
	testNo := 0
	for _, item := range plan.Cases {
		for k := 0; k < item.Counts; k++ {
			testNo++
			c, err := p.Reg.Invoke(item.Name)
			if err != nil {
				failNum++
				if rep != nil {
					rep.OnCaseStop(testNo, report.CaseReport{
						Seq:    testNo,
						Name:   item.Name,
						Title:  item.Title,
						Result: report.CaseFail,
						Error:  err.Error(),
						Extra:  cleanCaseArgs(item),
					})
				}
				finish(item.Title)
				return err
			}
			// Inject YAML args if the case supports it.
			if cfg, ok := c.(core.Configurable); ok {
				if err := cfg.Configure(item.Args); err != nil {
					failNum++
					if rep != nil {
						rep.OnCaseStop(testNo, report.CaseReport{
							Seq:    testNo,
							Name:   item.Name,
							Title:  item.Title,
							Result: report.CaseFail,
							Error:  err.Error(),
							Extra:  cleanCaseArgs(item),
						})
					}
					finish(item.Title)
					return fmt.Errorf("configure %s: %w", item.Name, err)
				}
			}
			if rep != nil {
				rep.OnCaseStart(testNo, report.CaseReport{
					Seq:    testNo,
					Name:   item.Name,
					Title:  item.Title,
					Result: report.CaseRunning,
					Extra:  cleanCaseArgs(item),
				})
			}
			// case_start 日志（对齐 Perl Runner.pm:119：Log::Any category
			// RUNNER 写 "case_start <name> " . yaml_dump(\%tmp_arg)，plan
			// args 以多行 YAML 追加到行尾）。args 为空时只输出 <name>，
			// 不带尾随空格。env.Log 可能为 nil（纯测试构造的 Env 常见），
			// 需 guard。
			if env.Log != nil {
				msg := "case_start " + item.Name
				if args := dumpArgsForLog(item.Args); args != "" {
					msg += " " + args
				}
				env.Log.Info("RUNNER", msg)
			}
			t0 := time.Now()
			runErr := c.Run(ctx, env)
			cr := report.CaseReport{
				Seq:   testNo,
				Name:  item.Name,
				Title: item.Title,
				Time:  time.Since(t0).Seconds(),
				Extra: cleanCaseArgs(item),
			}
			if runErr == nil {
				okNum++
				cr.Result = report.CaseOK
			} else {
				failNum++
				cr.Result = report.CaseFail
				cr.Error = runErr.Error()
			}
			// case_stop 日志（对齐 Perl Runner.pm:161：
			// "case_stop <name> <ok|fail> <time>"，time 为 %.2f 秒，category
			// RUNNER 与 case_start 一致，供报告端按 case 窗口切片日志）。
			if env.Log != nil {
				resultStr := "ok"
				if runErr != nil {
					resultStr = "fail"
				}
				env.Log.Info("RUNNER", fmt.Sprintf("case_stop %s %s %.2f", item.Name, resultStr, time.Since(t0).Seconds()))
			}
			if rep != nil {
				rep.OnCaseStop(testNo, cr)
			}
			if runErr != nil {
				finish(item.Title)
				return fmt.Errorf("case %s failed: %w", item.Name, runErr)
			}
		}
	}
	finish("")
	return nil
}

// dumpArgsForLog 把 plan args 序列化为 YAML 多行文本，追加在 case_start
// 日志行 `case_start <name> ` 之后（对齐 Perl Runner.pm:119 的
// yaml_dump(\%tmp_arg)）。args 为空（或序列化失败）时返回空串，调用方
// 不追加空格。yaml.v3 对 map 键按字节序排序输出，比 Perl hash 更确定。
func dumpArgsForLog(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	data, err := yaml.Marshal(args)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\n")
}

// cleanCaseArgs 深拷贝 plan item 的自定义 args 为 CaseReport.Extra，并按
// Perl DisplayRole.pm:69-88 app_reports 规则删冗余：desc == title 时 desc
// 不进入 case 条目（title 保留在独立字段）。返回 nil 表示无自定义键
// （YAML `,inline` 不输出任何东西）。counts/parallel/case_seq 是 CaseItem
// 的独立字段、不在 Args 里，天然不进 Extra。
func cleanCaseArgs(item config.CaseItem) map[string]any {
	extra := make(map[string]any, len(item.Args))
	for k, v := range item.Args {
		extra[k] = v
	}
	if item.Desc == item.Title {
		delete(extra, "desc")
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

// buildSummary derives the plan Summary from counters and the run start// time. StartTime/StopTime use RFC3339 so timestamps are unambiguous.
//
// Result 是 int（1/0），与 Perl ConfigRole 一致；reason 为空表示成功,
// 此时 Result=1, Reason="ok"；首个失败用例的 title 写入 Reason 并将 Result 置 0。
// TotalTime 仿 Perl DisplayRole test_stop: sprintf("%.2f", elapsed sec)。
func buildSummary(total, okNum, failNum int, start time.Time, reason string) report.Summary {
	sum := report.Summary{
		TotalNum:   total,
		PlanedNum:  total,
		RunningNum: 0,
		OKNum:      okNum,
		FailNum:    failNum,
		Result:     1,
		Reason:     "ok",
		TotalTime:  time.Since(start).Seconds(),
		StartTime:  start.Format(time.RFC3339),
		StopTime:   time.Now().Format(time.RFC3339),
	}
	if failNum > 0 || reason != "" {
		sum.Result = 0
		if reason != "" {
			sum.Reason = reason
		}
	}
	return sum
}
