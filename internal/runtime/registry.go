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
	"time"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/report"
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
				})
			}
			t0 := time.Now()
			runErr := c.Run(ctx, env)
			cr := report.CaseReport{
				Seq:   testNo,
				Name:  item.Name,
				Title: item.Title,
				Time:  time.Since(t0).Seconds(),
			}
			if runErr == nil {
				okNum++
				cr.Result = report.CaseOK
			} else {
				failNum++
				cr.Result = report.CaseFail
				cr.Error = runErr.Error()
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

// buildSummary derives the plan Summary from counters and the run start
// time. StartTime/StopTime use RFC3339 so timestamps are unambiguous.
func buildSummary(total, okNum, failNum int, start time.Time, reason string) report.Summary {
	sum := report.Summary{
		TotalNum:   total,
		PlanedNum:  total,
		RunningNum: total,
		OKNum:      okNum,
		FailNum:    failNum,
		Result:     "pass",
		StartTime:  start.Format(time.RFC3339),
		StopTime:   time.Now().Format(time.RFC3339),
		Duration:   time.Since(start).Seconds(),
	}
	if failNum > 0 || reason != "" {
		sum.Result = "fail"
		sum.Reason = reason
	}
	return sum
}
