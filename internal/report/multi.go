package report

import "time"

// MultiReporter fans every plan event out to all wrapped reporters, so a
// single run can produce several outputs (YAML file, TAP stream, UI
// callback) at once. Nil entries are skipped.
type MultiReporter struct {
	Reporters []Reporter
}

// NewMulti wraps zero or more reporters. Nil entries are ignored.
func NewMulti(rs ...Reporter) *MultiReporter {
	return &MultiReporter{Reporters: rs}
}

func (m *MultiReporter) OnPlanStart(total int, startTime time.Time) {
	for _, r := range m.Reporters {
		if r != nil {
			r.OnPlanStart(total, startTime)
		}
	}
}

func (m *MultiReporter) OnCaseStart(seq int, cr CaseReport) {
	for _, r := range m.Reporters {
		if r != nil {
			r.OnCaseStart(seq, cr)
		}
	}
}

func (m *MultiReporter) OnCaseStop(seq int, cr CaseReport) {
	for _, r := range m.Reporters {
		if r != nil {
			r.OnCaseStop(seq, cr)
		}
	}
}

func (m *MultiReporter) OnPlanStop(summary Summary) {
	for _, r := range m.Reporters {
		if r != nil {
			r.OnPlanStop(summary)
		}
	}
}
